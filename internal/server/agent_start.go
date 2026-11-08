package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"

	"sort"
	"time"

	"github.com/mash/kioku/internal/agentgen"
	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/prompts"
)

func validateGeneration(st model.State, job model.Job) error {
	if _, err := member(st, job.ProjectID, job.UserID); err != nil {
		return err
	}
	if err := checkJobAgent(st, job); err != nil {
		return err
	}
	current, ok := st.Jobs[job.ID]
	if !ok || current.Status != "running" || !sameManagedRun(current, job) {
		return fail(409, "generation job was canceled")
	}
	return nil
}

var errAgentBudgetTooSmall = errors.New("managed agent budget is below 50 cents")

func sameManagedRun(a, b model.Job) bool {
	if a.ID != b.ID {
		return false
	}
	if b.Run == nil {
		return a.Run == nil
	}
	return a.Run != nil && a.Run.Attempt.ID == b.Run.Attempt.ID
}
func (s *Server) startAgentJob(ctx context.Context, job model.Job) error {
	unlock, err := s.lockProject(ctx, "generate:"+job.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	if err = s.startAgentJobLocked(ctx, job); err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// The generation lock prevents a new attempt from appearing during setup.
	if viewErr := s.DB.View(func(st model.State) error { job = st.Jobs[job.ID]; return nil }); viewErr != nil {
		return viewErr
	}
	return s.failAgentJobLocked(cleanupCtx, job, err.Error(), nil)
}
func (s *Server) startAgentJobLocked(ctx context.Context, job model.Job) error {
	if s.runners == nil {
		return runner.ErrNotConfigured
	}
	r, err := s.runners.New(job)
	if err != nil {
		return err
	}
	var items []model.InboxItem
	var published model.Revision
	var incomplete bool
	var raw string
	err = s.DB.Update(func(st *model.State) error {
		if err := validateGeneration(*st, job); err != nil {
			return err
		}
		p := st.Projects[job.ProjectID]
		published = st.Revisions[p.Revision]
		incomplete = p.WikiIncomplete
		over := completedWikiBytes(published) > s.Config.MaxWikiBytes
		for _, item := range st.Inbox {
			if item.ProjectID == p.ID && item.RunID == "" && (!over || item.Status == "removed") {
				items = append(items, item)
			}
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Created.Equal(items[j].Created) {
				return items[i].ID < items[j].ID
			}
			return items[i].Created.Before(items[j].Created)
		})
		job = st.Jobs[job.ID]
		if len(items) == 0 {
			job.Status = "completed"
			job.Unchanged = true
			job.Revision = p.Revision
			st.Jobs[job.ID] = job
			return nil
		}
		budget := s.Config.AgentMaxSessionCents
		if job.AgentBudgetCents > 0 && job.AgentBudgetCents < budget {
			budget = job.AgentBudgetCents
		}
		if budget < 50 {
			return errAgentBudgetTooSmall
		}
		now := time.Now().UTC()
		run := &model.AgentRun{Phase: model.PhasePreparing, PreparedAt: now, Deadline: now.Add(s.Config.AgentJobTimeout), Attempt: model.Attempt{ID: auth.Token()[:20]}, BudgetCents: budget, BaseRevision: p.Revision}
		for i, item := range items {
			item.RunID = run.Attempt.ID
			items[i] = item
			st.Inbox[item.ID] = item
			run.InboxIDs = append(run.InboxIDs, item.ID)
		}
		var token model.AgentToken
		raw, token = issueJobToken(st, job, run.Deadline.Add(10*time.Minute))
		run.TokenID = token.ID
		job.Run = run
		st.Jobs[job.ID] = job
		return nil
	})
	if err != nil || job.Run == nil {
		return err
	}
	base := s.Config.BaseURL + "/api/projects/" + job.ProjectID + "/jobs/" + job.ID + "/agent/"
	spec := agentgen.InputSpec{JobID: job.ID, ProjectID: job.ProjectID, InputURL: base + "input", OutputURL: base + "output", Wiki: map[string]string{}}
	for _, item := range items {
		in := agentgen.InboxText{Item: item}
		if item.Status == "fetched" {
			b, err := s.Blobs.Get(ctx, item.BodyKey())
			if err != nil {
				return err
			}
			in.Text = string(b)
		}
		spec.Inbox = append(spec.Inbox, in)
	}
	spec.Wiki, err = s.generationWiki(ctx, published, incomplete)
	if err != nil {
		return err
	}
	var archive bytes.Buffer
	if err = agentgen.BuildInput(spec, &archive); err != nil {
		return err
	}
	if err = s.Blobs.Put(ctx, agentJobKey(job, "input.tar.gz"), archive.Bytes()); err != nil {
		return err
	}
	ref, err := r.Start(ctx, runner.RunSpec{Key: attempt(job).Key, InputURL: spec.InputURL, OutputURL: spec.OutputURL, Token: runner.Secret(raw), FirstMessage: s.Config.Prompts.User(prompts.JobVariables{JobID: spec.JobID, ProjectID: spec.ProjectID, InputURL: spec.InputURL, OutputURL: spec.OutputURL}), BudgetCents: job.Run.BudgetCents, Deadline: job.Run.Deadline})
	if errors.Is(err, runner.ErrTemporary) {
		return nil
	}
	if err != nil {
		return err
	}
	err = s.DB.Update(func(st *model.State) error {
		j := st.Jobs[job.ID]
		if !sameManagedRun(j, job) || j.Status != "running" {
			return nil
		}
		j.Run.Ref = ref
		if j.Run.Phase == model.PhasePreparing {
			j.Run.Phase = model.PhaseAgentRunning
		}
		st.Jobs[j.ID] = j
		return nil
	})
	if err != nil {
		slog.Warn("agent run save failed; reconciliation will locate it", "job", job.ID, "error", err)
	}
	return nil
}

// releaseInbox makes failed attempts retryable. A later waiting put takes
// precedence over a failed older item for the same exact URL.
func releaseInbox(st *model.State, job model.Job) {
	if job.Run == nil {
		return
	}
	for id, item := range st.Inbox {
		if item.ProjectID != job.ProjectID || item.RunID != job.Run.Attempt.ID {
			continue
		}
		replaced := false
		for _, newer := range st.Inbox {
			// All waiting items were pinned when this attempt began. A waiting
			// replacement was accepted later, regardless of wall-clock changes.
			if newer.ProjectID == item.ProjectID && newer.RunID == "" && newer.SourceURL == item.SourceURL {
				replaced = true
				break
			}
		}
		if replaced {
			delete(st.Inbox, id)
		} else {
			item.RunID = ""
			st.Inbox[id] = item
		}
	}
}
