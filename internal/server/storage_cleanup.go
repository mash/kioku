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

func sqlAttempt(j store.JobRow) runner.Attempt {
	ref := runner.RunRef{ID: store.ValueString(j.SessionID), Resources: map[string]string{}}
	if run := j.Data.Run; run != nil {
		ref.UsesProviderFiles = run.UsesProviderFiles
		ref.Resources["provider"] = store.ValueString(run.Provider)
		if run.UploadStarted {
			ref.Resources["upload_started"] = "true"
		}
		if run.InputFileID != nil {
			ref.Resources["input_file_id"] = *run.InputFileID
		}
	}
	return runner.Attempt{Key: runner.AttemptKey{JobID: j.ID, ProjectID: j.ProjectID}, Run: ref}
}
func runRefChanged(j store.JobRow, ref runner.RunRef) bool {
	run := j.Data.Run
	if run == nil {
		return false
	}
	return ref.ID != "" && (store.ValueString(j.SessionID) != ref.ID || store.ValueString(j.Phase) == "preparing") ||
		run.UsesProviderFiles != ref.UsesProviderFiles ||
		!equalOptional(run.Provider, store.NullableString(ref.Resources["provider"])) ||
		run.UploadStarted != (ref.Resources["upload_started"] == "true") ||
		!equalOptional(run.InputFileID, store.NullableString(ref.Resources["input_file_id"]))
}

func applyRunRef(j *store.JobRow, ref runner.RunRef) {
	if ref.ID != "" {
		j.SessionID = &ref.ID
		if store.ValueString(j.Phase) == "preparing" {
			j.Phase = store.NullableString("agent_running")
		}
	}
	j.Data.Run.UsesProviderFiles = ref.UsesProviderFiles
	j.Data.Run.Provider = store.NullableString(ref.Resources["provider"])
	j.Data.Run.UploadStarted = ref.Resources["upload_started"] == "true"
	j.Data.Run.InputFileID = store.NullableString(ref.Resources["input_file_id"])
}
func (o *storageOperations) cleanup(ctx context.Context, id string, status runner.Status, providerCleanup func(context.Context, runner.Attempt) error) (bool, error) {
	// The caller holds the project generation gate across inspection and
	// cleanup. Provider IO holds neither the content gate nor a SQL transaction.
	var j store.JobRow
	if err := o.db.Read(ctx, func(q *store.Queries) error { var e error; j, e = q.Job(id); return e }); err != nil {
		return false, err
	}
	if j.Data.Run == nil || j.CleanupComplete {
		return true, nil
	}
	if j.Status == "queued" || j.Status == "running" && j.GenerationSnapshot == nil {
		return false, nil
	}
	run := j.Data.Run
	stopped := status.State == runner.StateStopped || status.State == runner.StateNone && (o.now().UnixMilli() >= run.PreparedAt+300000 || run.UsesProviderFiles && !run.UploadStarted && run.AbortReason != nil)
	if !stopped && status.State == runner.StateNone {
		// Usage is committed only after stopping the attempt. A previous cleanup
		// may have deleted its last session before a later cleanup step failed.
		if err := o.db.Read(ctx, func(q *store.Queries) error {
			_, err := q.Usage(id)
			stopped = err == nil
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}); err != nil {
			return false, err
		}
	}
	if !stopped {
		return false, nil
	}
	if err := o.db.Write(ctx, func(q *store.Queries) error {
		current, e := q.Job(id)
		if e != nil {
			return e
		}
		if current.Data.Run == nil {
			return errors.New("attempt disappeared")
		}
		data := model.UsageValues{UserID: j.Data.UserID, UsageMeasurements: status.Usage}
		p, e := q.Project(j.ProjectID)
		if e != nil {
			return e
		}
		data.OrgID = p.OrgID
		return q.RecordUsage(store.UsageRow{ProjectID: j.ProjectID, JobID: id, RecordedAt: o.now().UnixMilli(), Data: data})
	}); err != nil {
		return false, err
	}
	if providerCleanup != nil {
		if err := providerCleanup(ctx, sqlAttempt(j)); err != nil {
			return false, err
		}
	}
	unlock, err := o.lock(ctx, j.ProjectID)
	if err != nil {
		return false, err
	}
	defer unlock()
	if err = o.db.Write(ctx, func(q *store.Queries) error {
		current, e := q.Job(id)
		if e != nil {
			return e
		}
		current.InputReady = false
		return q.PutJob(current)
	}); err != nil {
		return false, err
	}
	ioCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	keys, err := o.objects.List(ioCtx, "projects/"+j.ProjectID+"/.kioku/jobs/"+id+"/")
	if err != nil {
		return false, err
	}
	for _, key := range keys {
		if strings.HasSuffix(string(key), ".tar.gz") {
			if err = o.objects.Delete(ioCtx, key); err != nil {
				return false, err
			}
		}
	}
	cancel()
	err = o.db.Write(ctx, func(q *store.Queries) error {
		current, e := q.Job(id)
		if e != nil {
			return e
		}
		if current.CleanupComplete {
			return nil
		}
		usage, e := q.Usage(id)
		if e != nil {
			return e
		}
		if current.Data.ViewGeneration != nil {
			current.Data.ViewGeneration.Remaining = remainingViewBudget(current.Data.ViewGeneration.Remaining, usage.Data.CostCents)
		}
		current.CleanupComplete = true
		return q.PutJob(current)
	})
	return err == nil, err
}
