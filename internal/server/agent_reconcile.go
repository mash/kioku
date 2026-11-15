package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mash/kioku/internal/agentgen"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/runner/claudemanaged"
	"github.com/mash/kioku/internal/store"
)

func (s *Server) reconcileAgentJobs(ctx context.Context) error {
	var jobs []store.JobRow
	if err := s.Store.Read(ctx, func(q *store.Queries) error { var err error; jobs, err = q.ReconcileJobs(); return err }); err != nil {
		return err
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.reconcileAgentJob(ctx, job); err != nil {
			s.reportFatal(s.databaseError(ctx, err))
			if fatal := s.fatalError.Load(); fatal != nil {
				return fatal
			}
			slog.Error("agent reconciliation deferred", "job", job.ID, "error", err)
		}
	}
	return nil
}
func agentRunFinished(j store.JobRow) bool {
	return j.GenerationSnapshot != nil || j.Status == "completed" || j.Status == "failed" || j.Status == "skipped"
}
func (s *Server) reconcileAgentJob(ctx context.Context, job store.JobRow) error {
	unlock, err := s.lockProject(ctx, "generate:"+job.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.reconcileAgentJobLocked(ctx, job)
}
func (s *Server) reconcileAgentJobLocked(ctx context.Context, job store.JobRow) error {
	current, err := s.loadJob(ctx, job.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	job = current
	if job.Data.Run == nil || agentRunFinished(job) && valueBool(job.CleanupComplete) {
		return nil
	}
	r, err := s.runners.New(job)
	if err != nil {
		if !agentRunFinished(job) && time.Now().UnixMilli() > job.Data.Run.Deadline {
			return s.failAgentJobLocked(ctx, job, "managed agents unavailable; job timed out", nil)
		}
		return fmt.Errorf("%w: %v", runner.ErrTemporary, err)
	}
	status, inspectErr := r.Inspect(ctx, sqlAttempt(job))
	if inspectErr != nil {
		status = runner.Status{State: runner.StateUnknown}
	} else if status.Run.ID != "" {
		if err := s.saveDiscoveredRef(ctx, job.ID, status.Run); err != nil {
			return err
		}
		job, err = s.loadJob(ctx, job.ID)
		if err != nil {
			return err
		}
	}
	if agentRunFinished(job) {
		return s.cleanupAgentJob(ctx, job, &status)
	}
	if job.Data.Run.AbortReason != nil {
		return s.failAgentJobLocked(ctx, job, *job.Data.Run.AbortReason, &status)
	}
	authErr := s.Store.Read(ctx, func(q *store.Queries) error { return sqlJobAuthorization(q, job, time.Now().UnixMilli()) })
	if authErr != nil {
		var api apiError
		if !errors.As(authErr, &api) {
			return authErr
		}
		return s.failAgentJobLocked(ctx, job, authErr.Error(), &status)
	}
	if status.Reason == claudemanaged.MultipleSessionsReason {
		return s.failAgentJobLocked(ctx, job, status.Reason, &status)
	}
	switch status.State {
	case runner.StateStopped:
		files := job.Data.Run.UsesProviderFiles
		if (files || job.ParentID != nil) && status.Failed {
			reason := status.Reason
			if reason == "" {
				reason = "managed agent failed"
			}
			if job.ParentID != nil {
				reason = "View の生成処理に失敗しました"
			}
			return s.failAgentJobLocked(ctx, job, reason, &status)
		}
		if valueString(job.Phase) == "output_received" || valueString(job.Phase) == "finalizing" {
			return s.finalizeAgentJobLocked(ctx, job, status)
		}
		if files {
			return s.collectAgentOutput(ctx, r, job, status)
		}
		reason := status.Reason
		if job.ParentID != nil && reason != "" {
			reason = "View の生成処理に失敗しました"
		}
		if reason == "" {
			reason = "agent stopped without uploading output"
		}
		if job.ParentID != nil && job.Data.Run.OutputRejection != nil {
			reason = *job.Data.Run.OutputRejection
		}
		return s.finishFailedAgentJobLocked(ctx, job, reason, &status, true)
	case runner.StateNone:
		if time.Now().UnixMilli() >= job.Data.Run.PreparedAt+300000 || job.Data.Run.UsesProviderFiles && !job.Data.Run.UploadStarted {
			return s.failAgentJobLocked(ctx, job, "session creation did not finish", &status)
		}
	default:
		if time.Now().UnixMilli() > job.Data.Run.Deadline {
			return s.failAgentJobLocked(ctx, job, "agent job timed out", &status)
		}
	}
	if inspectErr != nil {
		return fmt.Errorf("%w: %v", runner.ErrTemporary, inspectErr)
	}
	return nil
}
func (s *Server) saveDiscoveredRef(ctx context.Context, id string, ref runner.RunRef) error {
	return s.Store.Write(ctx, func(q *store.Queries) error {
		j, err := q.Job(id)
		if err != nil {
			return err
		}
		if j.Data.Run == nil {
			return nil
		}
		if ref.ID != "" {
			j.SessionID = &ref.ID
			if valueString(j.Phase) == "preparing" {
				j.Phase = nullableString("agent_running")
			}
		}
		j.Data.Run.UsesProviderFiles = ref.UsesProviderFiles
		j.Data.Run.Provider = nullableString(ref.Resources["provider"])
		j.Data.Run.UploadStarted = ref.Resources["upload_started"] == "true"
		j.Data.Run.InputFileID = nullableString(ref.Resources["input_file_id"])
		return q.PutJob(j)
	})
}
func (s *Server) collectAgentOutput(ctx context.Context, r runner.Runner, job store.JobRow, status runner.Status) error {
	const timeoutReason = "managed agent output did not become available before the deadline"
	files, ok := r.(runner.FileRunner)
	if !ok || job.SessionID == nil || status.Run.ID != *job.SessionID {
		return s.failAgentJobLocked(ctx, job, "cannot identify managed agent output", &status)
	}
	job, expired, err := s.storage.beginFileWait(ctx, job.ID)
	if err != nil {
		return err
	}
	if expired {
		return s.failAgentJobLocked(ctx, job, timeoutReason, &status)
	}
	body, err := files.CollectOutput(ctx, sqlAttempt(job), status, s.agentOutputLimit(job))
	if fileOutputExpired(job.Data.Run, s.storage.now().UnixMilli()) {
		return s.failAgentJobLocked(ctx, job, timeoutReason, &status)
	}
	if errors.Is(err, runner.ErrOutputPending) || errors.Is(err, runner.ErrTemporary) {
		return nil
	}
	if err != nil {
		return s.failAgentJobLocked(ctx, job, err.Error(), &status)
	}
	check := func(q *store.Queries) error {
		j, err := s.storage.runningJob(q, job.ID)
		if err != nil {
			return err
		}
		if !j.Data.Run.UsesProviderFiles || !equalOptional(j.SessionID, job.SessionID) || valueString(j.Phase) != "agent_running" {
			return fail(409, "job no longer accepts managed agent output")
		}
		if fileOutputExpired(j.Data.Run, s.storage.now().UnixMilli()) {
			return fail(409, timeoutReason)
		}
		return nil
	}
	_, _, err = s.saveAgentOutput(ctx, job, body, check)
	if err != nil {
		var invalid *agentgen.OutputError
		var api apiError
		if errors.As(err, &invalid) || errors.As(err, &api) {
			return s.failAgentJobLocked(ctx, job, err.Error(), &status)
		}
		return err
	}
	return s.finalizeAgentJobLocked(ctx, job, status)
}
func (s *Server) finalizeAgentJobLocked(ctx context.Context, job store.JobRow, status runner.Status) error {
	if status.State != runner.StateStopped {
		return nil
	}
	proceed := false
	if err := s.Store.Write(ctx, func(q *store.Queries) error {
		j, err := s.storage.runningJob(q, job.ID)
		if err != nil {
			return err
		}
		if j.OutputHash == nil || (valueString(j.Phase) != "output_received" && valueString(j.Phase) != "finalizing") {
			return nil
		}
		j.Phase = nullableString("finalizing")
		job = j
		proceed = true
		return q.PutJob(j)
	}); err != nil {
		return err
	}
	if !proceed {
		return nil
	}
	unlock, err := s.lockProject(ctx, job.ProjectID)
	if err != nil {
		return err
	}
	body, err := s.Blobs.Get(ctx, sqlJobKey(job, "output-"+*job.OutputHash+".tar.gz"))
	unlock()
	if err != nil {
		return err
	}
	if string(model.HashContent(body)) != *job.OutputHash {
		return s.failAgentJobLocked(ctx, job, "agent output hash mismatch", &status)
	}
	if job.ParentID != nil {
		bundle, err := readViewRunnerOutput(body)
		if err != nil {
			return s.failAgentJobLocked(ctx, job, err.Error(), &status)
		}
		err = s.storage.publishView(ctx, job.ID, bundle)
		if errors.Is(err, sql.ErrNoRows) {
			if err = s.storage.finishView(ctx, job.ID, "skipped", viewDeleted); err != nil {
				return err
			}
		} else if err != nil {
			var api apiError
			if errors.As(err, &api) {
				return s.failAgentJobLocked(ctx, job, err.Error(), &status)
			}
			return err
		}
	} else {
		pages, err := agentgen.ReadAcceptedOutput(bytes.NewReader(body))
		if err != nil {
			return s.failAgentJobLocked(ctx, job, "stored agent output is invalid", &status)
		}
		if _, err = s.storage.publishWikiLocked(ctx, job, pages); err != nil {
			var api apiError
			if errors.As(err, &api) {
				return s.failAgentJobLocked(ctx, job, err.Error(), &status)
			}
			return err
		}
	}
	s.notify()
	return s.cleanupAgentJob(ctx, job, &status)
}
func (s *Server) failAgentJobLocked(ctx context.Context, job store.JobRow, reason string, status *runner.Status) error {
	return s.finishFailedAgentJobLocked(ctx, job, reason, status, false)
}
func (s *Server) finishFailedAgentJobLocked(ctx context.Context, job store.JobRow, reason string, status *runner.Status, onlyWithoutOutput bool) error {
	wins, err := s.storage.failJob(ctx, job.ID, reason, status, onlyWithoutOutput)
	if err != nil {
		return err
	}
	if wins {
		return s.finalizeAgentJobLocked(ctx, job, *status)
	}
	job, err = s.loadJob(ctx, job.ID)
	if err != nil {
		return err
	}
	if job.Data.Run == nil {
		s.notify()
		return nil
	}
	if agentRunFinished(job) {
		s.notify()
		return s.cleanupAgentJob(ctx, job, status)
	}
	r, err := s.runners.New(job)
	if err != nil {
		return fmt.Errorf("%w: %v", runner.ErrTemporary, err)
	}
	if err = r.Stop(ctx, sqlAttempt(job)); err != nil {
		return err
	}
	return fmt.Errorf("%w: waiting for agent session to stop", runner.ErrTemporary)
}
func (s *Server) cleanupAgentJob(ctx context.Context, job store.JobRow, status *runner.Status) error {
	current, err := s.loadJob(ctx, job.ID)
	if err != nil {
		return err
	}
	job = current
	if job.Data.Run == nil || valueBool(job.CleanupComplete) || !agentRunFinished(job) {
		return nil
	}
	r, err := s.runners.New(job)
	if err != nil {
		return fmt.Errorf("%w: %v", runner.ErrTemporary, err)
	}
	if status == nil {
		v, err := r.Inspect(ctx, sqlAttempt(job))
		if err != nil {
			return err
		}
		status = &v
	}
	if status.State == runner.StateRunning || status.State == runner.StateUnknown {
		if err := r.Stop(ctx, sqlAttempt(job)); err != nil {
			return err
		}
		return runner.ErrTemporary
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	complete, err := s.storage.cleanup(cleanupCtx, job.ID, *status, r.Cleanup)
	if err == nil && complete {
		s.notify()
	}
	return err
}
