package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"sort"
	"time"

	"github.com/mash/kioku/internal/agentgen"
	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/model"
)

type generationSnapshot struct {
	project   model.Project
	published model.Revision
	sources   []model.Source
}

func (s *Server) agentSnapshot(job model.Job) (generationSnapshot, error) {
	var in generationSnapshot
	err := s.DB.View(func(st model.State) error {
		var err error
		in.project, err = member(st, job.ProjectID, job.UserID)
		if err != nil {
			return err
		}
		if err = checkJobAgent(st, job); err != nil {
			return err
		}
		current, ok := st.Jobs[job.ID]
		if !ok || current.Status != "running" {
			return fail(409, "generation job was canceled")
		}
		in.published = st.Revisions[in.project.Revision]
		for _, src := range st.Sources {
			if src.ProjectID == job.ProjectID {
				in.sources = append(in.sources, src)
			}
		}
		return nil
	})
	if err != nil {
		return in, err
	}
	sort.Slice(in.sources, func(i, j int) bool {
		a, b := in.sources[i], in.sources[j]
		if a.Created.Equal(b.Created) {
			return a.ID < b.ID
		}
		return a.Created.Before(b.Created)
	})
	latest := map[string]string{}
	for _, src := range in.sources {
		if src.URL != "" {
			latest[src.URL] = src.ID
		}
	}
	for u, id := range in.project.SourceHeads {
		latest[u] = id
	}
	active := in.sources[:0]
	for _, src := range in.sources {
		if src.URL == "" || latest[src.URL] == src.ID {
			active = append(active, src)
		}
	}
	in.sources = active
	return in, nil
}
func validateGeneration(st model.State, job model.Job, sources []model.Source) error {
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
	for _, src := range sources {
		v, ok := st.Sources[src.ID]
		if !ok || v.ProjectID != job.ProjectID {
			return fail(409, "generation inputs were removed")
		}
	}
	return nil
}

var errAgentBudgetTooSmall = errors.New("managed agent budget is below 50 cents")

func sameManagedRun(a, b model.Job) bool {
	if a.ID != b.ID {
		return false
	}
	if b.Managed == nil {
		return a.Managed == nil
	}
	return a.Managed != nil && a.Managed.Attempt.ID == b.Managed.Attempt.ID
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
	return s.failAgentJobLocked(cleanupCtx, job, err.Error(), false)
}
func (s *Server) startAgentJobLocked(ctx context.Context, job model.Job) error {
	in, err := s.agentSnapshot(job)
	if err != nil {
		return err
	}
	p := in.project
	agentID, agentVersion, environmentID := s.Config.MAAgentID, s.Config.MAAgentVersion, s.Config.MAEnvironmentID
	budgetLimit := s.Config.AgentMaxSessionCents
	if job.AnthropicConnectionID != "" {
		c, err := s.jobConnection(job)
		if err != nil {
			return err
		}
		if c.State != "ready" {
			return errors.New("Anthropic connection is not ready")
		}
		agentID, agentVersion, environmentID = c.AgentID, c.AgentVersion, c.EnvironmentID
		budgetLimit = job.AgentBudgetCents
	}

	if !job.Full && !p.WikiIncomplete && (in.published.ID != "" && in.published.ProjectID == p.ID && sameSourceIDs(in.sources, in.published.SourceIDs) || p.Revision == "" && len(in.sources) == 0 && p.SourceVersion == p.WikiSourceVersion) {
		unlock, err := s.lockProject(ctx, p.ID)
		if err != nil {
			return err
		}
		defer unlock()
		return s.DB.Update(func(st *model.State) error {
			if err := validateGeneration(*st, job, in.sources); err != nil {
				return err
			}
			current := st.Projects[p.ID]
			current.WikiSourceVersion = p.SourceVersion
			current.ProcessedIntakeSequence = p.NextIntakeSequence
			st.Projects[p.ID] = current
			j := st.Jobs[job.ID]
			j.Status = "completed"
			j.Unchanged = true
			j.Revision = p.Revision
			j.SourceVersion = p.SourceVersion
			j.Fingerprint = in.published.Fingerprint
			st.Jobs[j.ID] = j
			return nil
		})
	}
	mode := model.ModeIncremental
	if job.Full || p.Revision == "" {
		mode = model.ModeFull
	}
	base := s.Config.BaseURL + "/api/projects/" + p.ID + "/jobs/" + job.ID + "/agent/"
	spec := agentgen.InputSpec{Mode: mode, JobID: job.ID, ProjectID: p.ID, PublishedRevision: p.Revision, InputURL: base + "input", OutputURL: base + "output", Wiki: map[string]string{}}
	published := []model.Source{}
	if err = s.DB.View(func(st model.State) error {
		for _, id := range in.published.SourceIDs {
			if src, ok := st.Sources[id]; ok {
				published = append(published, src)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	spec.Changes = agentgen.DiffChanges(published, in.sources)
	for _, src := range in.sources {
		b, err := s.Blobs.Get(ctx, src.TextKey)
		if err != nil {
			return err
		}
		spec.Sources = append(spec.Sources, agentgen.SourceText{Source: src, Text: string(b)})
	}
	if mode == model.ModeIncremental {
		for _, change := range spec.Changes.Updated {
			b, err := s.Blobs.Get(ctx, change.Previous.TextKey)
			if err != nil {
				return err
			}
			spec.Previous = append(spec.Previous, agentgen.SourceText{Source: change.Previous, Text: string(b)})
		}
		for name, key := range in.published.Files {
			if agentgen.AgentPage(name) {
				b, err := s.Blobs.Get(ctx, key)
				if err != nil {
					return err
				}
				spec.Wiki[name] = string(b)
			}
		}
	}
	var archive bytes.Buffer
	if err = agentgen.BuildInput(spec, &archive); err != nil {
		return err
	}
	var raw string
	err = s.DB.Update(func(st *model.State) error {
		if err := validateGeneration(*st, job, in.sources); err != nil {
			return err
		}
		now := time.Now().UTC()
		if job.AnthropicConnectionID != "" {
			c := st.Users[job.UserID].Anthropic
			if c == nil || c.ID != job.AnthropicConnectionID || c.State != "ready" {
				return errors.New("Anthropic connection is not ready")
			}
		}
		// The budget caps the managed agent session. It is not a charge.
		budget := budgetLimit
		if budget < 50 {
			return errAgentBudgetTooSmall
		}
		attempt := model.Attempt{ID: auth.Token()[:20]}
		run := &model.ManagedRun{Phase: model.PhasePreparing, Mode: mode, PreparedAt: now, Deadline: now.Add(s.Config.AgentJobTimeout), Attempt: attempt, BudgetCents: budget, MAEnvironmentID: environmentID, MAAgentID: agentID, MAAgentVersion: agentVersion, BaseRevision: p.Revision, SourceVersion: p.SourceVersion, NextIntakeSequence: p.NextIntakeSequence, SourcesUpdatedAt: p.SourcesUpdatedAt}
		for _, src := range in.sources {
			run.SourceIDs = append(run.SourceIDs, src.ID)
		}
		var token model.AgentToken
		raw, token = issueGeneratorToken(st, job, run.Deadline.Add(10*time.Minute))
		run.TokenID = token.ID
		job = st.Jobs[job.ID]
		job.Managed = run
		st.Jobs[job.ID] = job
		return nil
	})
	if err != nil {
		return err
	}
	// Persist cleanup identity before the first external artifact can exist.
	if err = s.Blobs.Put(ctx, agentJobKey(job, "input.tar.gz"), archive.Bytes()); err != nil {
		return err
	}
	u, _ := url.Parse(s.Config.BaseURL)
	vault, createErr := s.managedClient(job).CreateJobVault(ctx, managedagents.VaultInput{JobID: job.ID, AttemptID: job.Managed.Attempt.ID, SecretName: "KIOKU_GENERATOR_TOKEN", SecretValue: raw, AllowedHost: u.Hostname()})
	if vault != "" {
		if err = s.DB.Update(func(st *model.State) error {
			j := st.Jobs[job.ID]
			if !sameManagedRun(j, job) || j.Status != "running" {
				return fail(409, "generation job was canceled")
			}
			j.Managed.VaultID = vault
			st.Jobs[j.ID] = j
			return nil
		}); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_ = s.managedClient(job).ArchiveVault(cleanupCtx, vault)
			cancel()
			return err
		}
	}
	if createErr != nil {
		return createErr
	}
	id, err := s.managedClient(job).CreateSession(ctx, managedagents.SessionInput{MAAgentID: agentID, MAAgentVersion: agentVersion, EnvironmentID: environmentID, VaultID: vault, BudgetCents: job.Managed.BudgetCents, Title: "Kioku wiki " + job.ID, Metadata: map[string]string{"kioku_project_id": p.ID, "kioku_job_id": job.ID, "kioku_attempt_id": job.Managed.Attempt.ID}, FirstMessage: agentgen.FirstMessage(spec)})
	if err != nil {
		if errors.Is(err, managedagents.ErrTemporary) {
			slog.Warn("agent session creation uncertain; reconciliation will locate it", "job", job.ID)
			return nil
		}
		return err
	}
	accepted := false
	err = s.DB.Update(func(st *model.State) error {
		j := st.Jobs[job.ID]
		if !sameManagedRun(j, job) || j.Status != "running" || j.Managed.Phase != model.PhasePreparing && j.Managed.Phase != model.PhaseOutputReceived {
			return nil
		}
		j.Managed.SessionID = id
		if j.Managed.Phase == model.PhasePreparing {
			j.Managed.Phase = model.PhaseAgentRunning
		}
		st.Jobs[j.ID] = j
		accepted = true
		return nil
	})
	if !accepted {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = s.managedClient(job).Interrupt(cleanupCtx, id)
		_ = s.managedClient(job).ArchiveSession(cleanupCtx, id)
		_ = s.managedClient(job).ArchiveVault(cleanupCtx, vault)
	}
	return err
}
