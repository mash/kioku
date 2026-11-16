package server

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
)

var errPreparationRecorded = errors.New("input preparation failure recorded")

type storageInput struct {
	Job     store.JobRow
	Project store.ProjectRow
	Inbox   []store.InboxRow
	Wiki    []store.WikiFileRow
	View    *store.ViewRow
}

func (o *storageOperations) prepareInput(ctx context.Context, id string, file bool, build func(context.Context, storageInput) ([]byte, error), deferred ...string) (store.JobRow, error) {
	var input storageInput
	if err := o.db.Read(ctx, func(q *store.Queries) error { var err error; input.Job, err = q.Job(id); return err }); err != nil {
		return store.JobRow{}, err
	}
	unlock, err := o.lock(ctx, input.Job.ProjectID)
	if err != nil {
		return input.Job, err
	}
	defer unlock()
	check := func(q *store.Queries) error {
		j, err := q.Job(id)
		if err != nil {
			return err
		}
		if j.Status != "queued" || j.Data.Run != nil {
			return fail(409, "generation job was canceled")
		}
		if err = sqlJobAuthorization(q, j, o.now().UnixMilli()); err != nil {
			return err
		}
		next, err := q.NextQueued(o.config.AgentMaxSessions, deferred...)
		if err != nil {
			return err
		}
		if next.ID != id {
			return fail(409, "generation slot unavailable")
		}
		p, err := q.Project(j.ProjectID)
		if err != nil {
			return err
		}
		if j.ParentID != nil {
			parent, err := q.Job(*j.ParentID)
			if err != nil {
				return err
			}
			if parent.Status != "running" || parent.ProjectID != j.ProjectID || j.Data.ViewTask == nil || parent.GenerationSnapshot == nil || !equalOptional(p.WikiRevision, parent.GenerationSnapshot) {
				return fail(409, "Wiki または生成ジョブが変更されました")
			}
			v, err := q.View(p.ID, j.Data.ViewTask.ViewID)
			if errors.Is(err, sql.ErrNoRows) {
				return fail(404, viewDeleted)
			}
			if err != nil {
				return err
			}
			input.View = &v
		}
		input.Job, input.Project = j, p
		return nil
	}
	if err = o.db.Read(ctx, func(q *store.Queries) error {
		if err := check(q); err != nil {
			return err
		}
		var err error
		input.Wiki, err = q.ProjectWiki(input.Project.ID)
		if err != nil {
			return err
		}
		if input.Job.ParentID == nil {
			size, err := wikiBytes(input.Wiki)
			if err != nil {
				return err
			}
			input.Inbox, err = q.AvailableInbox(input.Project.ID, size > o.config.MaxWikiBytes)
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		var api apiError
		if errors.As(err, &api) && api.status == 404 && api.message == viewDeleted {
			return input.Job, o.finishView(ctx, id, "skipped", viewDeleted)
		}
		if errors.As(err, &api) {
			return input.Job, o.failPreparation(ctx, id, err)
		}
		return input.Job, err
	}
	if input.Job.ParentID == nil && len(input.Inbox) == 0 {
		err = o.db.Write(ctx, func(q *store.Queries) error {
			if err := check(q); err != nil {
				return err
			}
			j := input.Job
			j.Status = "completed"
			j.Data.Unchanged = true
			j.Data.Revision = input.Project.WikiRevision
			if err := beginSQLViews(q, &j, input.Project, o.jobBudget(j)); err != nil {
				return err
			}
			input.Job = j
			return q.PutJob(j)
		})
		return input.Job, err
	}
	budget := o.jobBudget(input.Job)
	if budget < 50 {
		return input.Job, o.failPreparation(ctx, id, errAgentBudgetTooSmall)
	}
	ioCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	body, err := build(ioCtx, input)
	if err == nil {
		err = o.objects.Put(ioCtx, sqlJobKey(input.Job, "input.tar.gz"), body)
	}
	cancel()
	if err != nil {
		return input.Job, o.failPreparation(ctx, id, err)
	}
	selected := input
	err = o.db.Write(ctx, func(q *store.Queries) error {
		if err := check(q); err != nil {
			return err
		}
		if !equalOptional(input.Project.WikiRevision, selected.Project.WikiRevision) {
			return fail(409, "Wiki または生成ジョブが変更されました")
		}
		j := input.Job
		now := o.now()
		phase := "preparing"
		j.Status = "running"
		j.Phase = &phase
		j.CleanupComplete = false
		j.Data.Run = &store.RunData{UsesProviderFiles: file, PreparedAt: now.UnixMilli(), Deadline: now.Add(o.config.AgentJobTimeout).UnixMilli(), BudgetCents: budget}
		if j.Data.WikiStatus != nil {
			j.Data.WikiStatus = store.NullableString("running")
		}
		for _, item := range selected.Inbox {
			current, err := q.Inbox(item.ID)
			if err != nil {
				return err
			}
			if current.ProjectID != j.ProjectID || current.RunID != nil || current.SourceURL != item.SourceURL {
				return fail(409, "generation input changed")
			}
			current.RunID = &j.ID
			if err = q.PutInbox(current); err != nil {
				return err
			}
		}
		input.Job = j
		return q.PutJob(j)
	})
	return input.Job, err
}
func (o *storageOperations) jobBudget(j store.JobRow) int64 {
	if j.Data.AgentBudgetCents > 0 && j.Data.AgentBudgetCents < o.config.AgentMaxSessionCents {
		return j.Data.AgentBudgetCents
	}
	return o.config.AgentMaxSessionCents
}
func (o *storageOperations) failPreparation(ctx context.Context, id string, cause error) error {
	if err := o.db.Write(ctx, func(q *store.Queries) error {
		j, err := q.Job(id)
		if err != nil {
			return err
		}
		if j.Status != "queued" || j.Data.Run != nil {
			return nil
		}
		j.Status = "failed"
		j.Data.Error = store.NullableString(cause.Error())
		if j.Data.WikiStatus != nil {
			j.Data.WikiStatus = store.NullableString("failed")
		}
		return q.PutJob(j)
	}); err != nil {
		return err
	}
	return errPreparationRecorded
}
func (o *storageOperations) saveRunRef(ctx context.Context, id string, ref runner.RunRef) error {
	return o.db.Write(ctx, func(q *store.Queries) error {
		j, err := o.runningJob(q, id)
		if err != nil {
			return err
		}
		if j.SessionID != nil && ref.ID != "" && *j.SessionID != ref.ID {
			return fail(409, "generation attempt changed")
		}
		applyRunRef(&j, ref)
		return q.PutJob(j)
	})
}
func (o *storageOperations) saveOutput(ctx context.Context, id string, body []byte, authorize func(*store.Queries) (store.JobRow, error), commit ...func(*store.Queries) error) (string, error) {
	var j store.JobRow
	if err := o.db.Read(ctx, func(q *store.Queries) error { var err error; j, err = q.Job(id); return err }); err != nil {
		return "", err
	}
	unlock, err := o.lock(ctx, j.ProjectID)
	if err != nil {
		return "", err
	}
	defer unlock()
	check := func(q *store.Queries) error {
		current, err := o.outputJob(q, id, authorize)
		if err != nil {
			return err
		}
		if current.Phase == nil || store.ValueString(current.Phase) == "finalizing" || current.GenerationSnapshot != nil {
			return fail(409, "job no longer accepts output")
		}
		j = current
		return nil
	}
	if err = o.db.Read(ctx, check); err != nil {
		return "", err
	}
	hash := string(model.HashContent(body))
	changed := store.ValueString(j.OutputHash) != hash
	if changed {
		prefix := "projects/" + j.ProjectID + "/.kioku/jobs/" + id + "/"
		keys, err := o.objects.List(ctx, prefix)
		if err != nil {
			return "", err
		}
		for _, key := range keys {
			name := strings.TrimPrefix(string(key), prefix)
			if strings.HasPrefix(name, "output-") && strings.HasSuffix(name, ".tar.gz") && name != "output-"+store.ValueString(j.OutputHash)+".tar.gz" {
				if err = o.objects.Delete(ctx, key); err != nil {
					return "", fail(503, "previous output cleanup pending")
				}
			}
		}
		if err = o.objects.Put(ctx, sqlJobKey(j, "output-"+hash+".tar.gz"), body); err != nil {
			return "", err
		}
	}
	err = o.db.Write(ctx, func(q *store.Queries) error {
		if err := check(q); err != nil {
			return err
		}
		if changed {
			j.OutputHash = &hash
			j.Phase = store.NullableString("output_received")
			j.Data.Run.OutputRejection = nil
			if err := q.PutJob(j); err != nil {
				return err
			}
		}
		for _, fn := range commit {
			if err := fn(q); err != nil {
				return err
			}
		}
		return nil
	})
	return hash, err
}
func (o *storageOperations) beginFileWait(ctx context.Context, id string) (store.JobRow, bool, error) {
	var j store.JobRow
	expired := false
	err := o.db.Write(ctx, func(q *store.Queries) error {
		var err error
		j, err = o.runningJob(q, id)
		if err != nil {
			return err
		}
		if !j.Data.Run.UsesProviderFiles || store.ValueString(j.Phase) != "agent_running" {
			return fail(409, "job no longer accepts managed agent output")
		}
		now := o.now().UnixMilli()
		if j.Data.Run.OutputWaitSince == nil {
			j.Data.Run.OutputWaitSince = &now
		}
		expired = fileOutputExpired(j.Data.Run, now)
		return q.PutJob(j)
	})
	return j, expired, err
}
func fileOutputExpired(run *store.RunData, now int64) bool {
	return run.OutputWaitSince != nil && now >= min(*run.OutputWaitSince+90000, run.Deadline)
}
func (o *storageOperations) failJob(ctx context.Context, id, reason string, status *runner.Status, onlyWithoutOutput bool) (bool, error) {
	outputWins := false
	err := o.db.Write(ctx, func(q *store.Queries) error {
		j, err := q.Job(id)
		if err != nil {
			return err
		}
		if j.Status != "running" && j.Status != "queued" {
			return nil
		}
		if j.Data.Run != nil {
			run := j.Data.Run
			if onlyWithoutOutput && !run.UsesProviderFiles && run.AbortReason == nil && (j.ParentID == nil || status == nil || !status.Failed) && (store.ValueString(j.Phase) == "output_received" || store.ValueString(j.Phase) == "finalizing") {
				if err = sqlJobAuthorization(q, j, o.now().UnixMilli()); err == nil {
					outputWins = true
					return nil
				}
				var api apiError
				if !errors.As(err, &api) {
					return err
				}
				reason = err.Error()
			}
			stopped := status != nil && (status.State == runner.StateStopped || status.State == runner.StateNone && (o.now().UnixMilli() >= run.PreparedAt+300000 || run.UsesProviderFiles && !run.UploadStarted))
			if run.AbortReason == nil {
				run.AbortReason = store.NullableString(reason)
			} else {
				reason = *run.AbortReason
			}
			if !stopped && o.now().UnixMilli() <= run.Deadline+600000 {
				return q.PutJob(j)
			}
		}
		j.Status = "failed"
		j.Data.Error = store.NullableString(reason)
		if j.Data.WikiStatus != nil {
			j.Data.WikiStatus = store.NullableString("failed")
		}
		if err = q.ReleaseInbox(j.ProjectID, j.ID); err != nil {
			return err
		}
		return q.PutJob(j)
	})
	return outputWins, err
}
