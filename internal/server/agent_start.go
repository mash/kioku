package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mash/kioku/internal/agentgen"
	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/prompts"
)

var errAgentBudgetTooSmall = errors.New("managed agent budget is below 50 cents")

func (s *Server) loadJob(ctx context.Context, id string) (store.JobRow, error) {
	var j store.JobRow
	err := s.Store.Read(ctx, func(q *store.Queries) error { var err error; j, err = q.Job(id); return err })
	return j, err
}
func (s *Server) startAgentJob(ctx context.Context, job store.JobRow, deferred ...string) error {
	unlock, err := s.storage.lock(ctx, "generate:"+job.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	if s.runners == nil {
		return s.failUnstartedJob(ctx, job.ID, runner.ErrNotConfigured)
	}
	r, err := s.runners.New(job)
	if err != nil {
		return s.failUnstartedJob(ctx, job.ID, err)
	}
	files, offline := r.(runner.FileRunner)
	provider := ""
	if offline {
		provider = files.Provider()
	}
	var archive []byte
	var spec agentgen.InputSpec
	job, err = s.storage.prepareInput(ctx, job.ID, offline, provider, func(ctx context.Context, in storageInput) ([]byte, error) {
		spec = agentgen.InputSpec{JobID: in.Job.ID}
		if err := s.Store.Read(ctx, func(q *store.Queries) error {
			org, err := q.Organization(in.Project.OrgID)
			if err != nil {
				return err
			}
			spec.Namespace = org.Slug + "/" + in.Project.Slug
			if in.View != nil {
				prompt := store.ValueString(in.View.Data.Prompt)
				spec.ViewPrompt = &prompt
			} else {
				v, err := q.ViewBySlug(in.Project.ID, wikiViewSlug)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				prompt := store.ValueString(v.Data.Prompt)
				if prompt == "" {
					prompt = s.defaultWikiPrompt()
				}
				spec.WikiPrompt = &prompt
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if !offline {
			base := s.Config.BaseURL + "/api/projects/" + spec.Namespace + "/jobs/" + in.Job.ID + "/agent/"
			spec.InputURL, spec.OutputURL = base+"input", base+"output"
		}
		for _, item := range in.Inbox {
			input := agentgen.InboxText{Item: inboxProjection(item)}
			if !item.Remove {
				body, err := s.Blobs.Get(ctx, sqlInboxKey(item))
				if err != nil {
					return nil, err
				}
				input.Text = string(body)
			}
			spec.Inbox = append(spec.Inbox, input)
		}
		spec.Wiki = map[string]string{}
		for _, f := range in.Wiki {
			if !agentgen.AgentPage(f.Path) {
				continue
			}
			body, err := s.Blobs.Get(ctx, model.WikiContentKey(in.Project.ID, model.ContentHash(f.Hash)))
			if err != nil {
				return nil, err
			}
			if string(model.HashContent(body)) != f.Hash || int64(len(body)) != f.Data.Bytes {
				return nil, errors.New("wiki content integrity check failed")
			}
			spec.Wiki[f.Path] = string(body)
		}
		var buf bytes.Buffer
		if err := agentgen.BuildInput(spec, &buf); err != nil {
			return nil, err
		}
		archive = buf.Bytes()
		return archive, nil
	}, deferred...)
	if errors.Is(err, errPreparationRecorded) {
		return nil
	}
	if err != nil {
		return err
	}
	job, err = s.loadJob(ctx, job.ID)
	if err != nil {
		return err
	}
	if job.Data.Run == nil {
		s.notify()
		return nil
	}
	raw := ""
	if !offline {
		raw, err = s.Cipher.SealJobCredential(auth.JobCredential{JobID: job.ID, Expires: job.Data.Run.Deadline + 600000})
		if err != nil {
			return s.failAgentJobLocked(ctx, job, err.Error(), nil)
		}
	}
	runSpec := runner.RunSpec{Key: sqlAttempt(job).Key, InputURL: spec.InputURL, OutputURL: spec.OutputURL, Token: runner.Secret(raw), BudgetCents: job.Data.Run.BudgetCents, Deadline: storageDate(job.Data.Run.Deadline)}
	vars := prompts.JobVariables{JobID: spec.JobID, Namespace: spec.Namespace, InputURL: spec.InputURL, OutputURL: spec.OutputURL}
	if offline {
		vars.InputURL, vars.OutputURL = files.FilePaths()
		runSpec.InputArchive = archive
		runSpec.FirstMessage = s.Config.Prompts.FileUser(vars)
		runSpec.SaveRef = func(ref runner.RunRef) error {
			return s.storage.saveRunRef(ctx, job.ID, ref)
		}
	} else {
		runSpec.FirstMessage = s.Config.Prompts.User(vars)
	}
	if job.ParentID != nil {
		runSpec.System = viewRunnerSystem
		runSpec.FirstMessage = viewRunnerMessage(vars.InputURL, vars.OutputURL, offline)
	}
	startCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	ref, err := r.Start(startCtx, runSpec)
	cancel()
	if errors.Is(err, runner.ErrTemporary) {
		return err
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		current, e := s.loadJob(ctx, job.ID)
		if e != nil {
			return e
		}
		var status *runner.Status
		if current.Data.Run.UsesProviderFiles && !current.Data.Run.UploadStarted {
			status = &runner.Status{State: runner.StateStopped}
		}
		return s.failAgentJobLocked(ctx, current, err.Error(), status)
	}
	return s.storage.saveRunRef(ctx, job.ID, ref)
}
func inboxProjection(row store.InboxRow) model.InboxItem {
	return model.InboxItem{ID: row.ID, ProjectID: row.ProjectID, SourceURL: row.SourceURL, Remove: row.Remove, Submitter: row.Data.Submitter, Created: storageDate(row.Created), RunID: store.ValueString(row.RunID)}
}

func (s *Server) failUnstartedJob(ctx context.Context, id string, cause error) error {
	err := s.storage.failPreparation(ctx, id, cause)
	if errors.Is(err, errPreparationRecorded) {
		return nil
	}
	return err
}
