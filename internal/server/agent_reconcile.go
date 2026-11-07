package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/agentgen"
	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/wiki"
)

func (s *Server) reconcileAgentJobs(ctx context.Context) error {
	var jobs []model.Job
	if err := s.DB.View(func(st model.State) error {
		for _, j := range st.Jobs {
			if j.Run != nil && (j.Status == "running" || ((j.Status == "completed" || j.Status == "failed") && !j.Run.CleanupComplete)) {
				jobs = append(jobs, j)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	for _, job := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.reconcileAgentJob(ctx, job); err != nil {
			slog.Error("agent reconciliation deferred", "job", job.ID, "error", err)
		}
	}
	return nil
}
func (s *Server) reconcileAgentJob(ctx context.Context, job model.Job) error {
	unlock, err := s.lockProject(ctx, "generate:"+job.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	var current model.Job
	if err = s.DB.View(func(st model.State) error { current = st.Jobs[job.ID]; return nil }); err != nil {
		return err
	}
	if current.Run == nil || !sameManagedRun(current, job) {
		return nil
	}
	if current.Status != "running" && ((current.Status != "completed" && current.Status != "failed") || current.Run.CleanupComplete) {
		return nil
	}
	job = current
	r, err := s.runners.New(job)
	if err != nil {
		if job.Status == "running" && time.Now().After(job.Run.Deadline) {
			return s.DB.Update(func(st *model.State) error {
				j := st.Jobs[job.ID]
				if j.Status == "running" && sameManagedRun(j, job) {
					revokeJobToken(st, j.ID)
					j.Status, j.Error = "failed", "managed agents unavailable; job timed out"
					j.Run.CostCents = nil
					st.Jobs[j.ID] = j
				}
				return nil
			})
		}
		return nil
	}
	status, inspectErr := r.Inspect(ctx, attempt(job))
	if inspectErr != nil {
		status = runner.Status{State: runner.StateUnknown}
		if !errors.Is(inspectErr, runner.ErrTemporary) {
			slog.Warn("agent inspect failed", "job", job.ID, "error", inspectErr)
		}
	} else if status.Run.ID != "" && status.Run.ID != job.Run.Ref.ID {
		if err := s.DB.Update(func(st *model.State) error {
			j := st.Jobs[job.ID]
			if !sameManagedRun(j, job) {
				return nil
			}
			j.Run.Ref = status.Run
			if j.Run.Phase == model.PhasePreparing {
				j.Run.Phase = model.PhaseAgentRunning
			}
			st.Jobs[j.ID] = j
			job = j
			return nil
		}); err != nil {
			return err
		}
		slog.Info("agent run located by key", "job", job.ID)
	}
	if job.Status == "completed" || job.Status == "failed" {
		return s.cleanupAgentJob(ctx, job, &status)
	}
	if job.Run.AbortReason != "" {
		return s.failAgentJobLocked(ctx, job, job.Run.AbortReason, &status)
	}
	switch status.State {
	case runner.StateStopped:
		if job.Run.Phase == model.PhaseOutputReceived || job.Run.Phase == model.PhaseFinalizing {
			return s.finalizeAgentJobLocked(ctx, job, status)
		}
		reason := status.Reason
		if reason == "" {
			reason = "agent stopped without uploading output"
		}
		return s.finishFailedAgentJobLocked(ctx, job, reason, &status, true)
	case runner.StateNone:
		if !time.Now().Before(job.Run.PreparedAt.Add(5 * time.Minute)) {
			return s.failAgentJobLocked(ctx, job, "session creation did not finish", &status)
		}
	default:
		if time.Now().After(job.Run.Deadline) {
			return s.failAgentJobLocked(ctx, job, "agent job timed out", &status)
		}
	}
	return nil
}
func (s *Server) finalizeAgentJobLocked(ctx context.Context, job model.Job, status runner.Status) error {
	if status.State != runner.StateStopped {
		return nil
	}
	proceed := false
	if err := s.DB.Update(func(st *model.State) error {
		j := st.Jobs[job.ID]
		if j.Status != "running" || j.Run == nil || !sameManagedRun(j, job) {
			return nil
		}
		if j.Run.Phase != model.PhaseOutputReceived && j.Run.Phase != model.PhaseFinalizing {
			return nil
		}
		j.Run.Phase = model.PhaseFinalizing
		st.Jobs[j.ID] = j
		job = j
		proceed = true
		return nil
	}); err != nil {
		return err
	}
	if !proceed {
		return nil
	}
	failJob := func(reason string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.failAgentJobLocked(ctx, job, reason, &status)
	}
	body, err := s.Blobs.Get(ctx, job.Run.OutputKey)
	if err != nil {
		return failJob("could not read agent output")
	}
	if auth.Hash(string(body)) != job.Run.OutputHash {
		return failJob("agent output hash mismatch")
	}
	pages, err := agentgen.ReadOutput(bytes.NewReader(body), agentgen.DefaultOutputLimits())
	if err != nil {
		return failJob("stored agent output is invalid")
	}
	inputs, err := s.managedInputs(ctx, job)
	if err != nil {
		return failJob(err.Error())
	}
	files, err := wiki.Assemble(inputs, pages)
	if err != nil {
		return failJob(err.Error())
	}
	var project model.Project
	if err = s.DB.View(func(st model.State) error { project = st.Projects[job.ProjectID]; return nil }); err != nil {
		return err
	}
	project.SourceVersion = job.Run.SourceVersion
	project.NextIntakeSequence = job.Run.NextIntakeSequence
	project.SourcesUpdatedAt = job.Run.SourcesUpdatedAt
	sources := make([]model.Source, 0, len(inputs))
	for _, in := range inputs {
		sources = append(sources, in.Source)
	}
	_, err = s.publishWiki(ctx, publishInput{Job: job, Project: project, Sources: sources, Files: files, Fingerprint: agentFingerprint(inputs), Validate: func(st model.State) error { return validateGeneration(st, job, sources) }, Finish: func(st *model.State, now time.Time) error {
		revokeJobToken(st, job.ID)
		j := st.Jobs[job.ID]
		j.Run.CostCents = status.Cost
		st.Jobs[j.ID] = j
		return nil
	}})
	if err != nil {
		return failJob(err.Error())
	}
	return s.cleanupAgentJob(ctx, job, &status)
}
func agentFingerprint(inputs []wiki.Input) string {
	type item struct{ ID, Hash string }
	v := struct {
		Version string
		Sources []item
	}{Version: "agent-v1"}
	for _, in := range inputs {
		v.Sources = append(v.Sources, item{in.Source.ID, in.Source.ContentHash})
	}
	b, _ := json.Marshal(v)
	return auth.Hash(string(b))
}

func (s *Server) failAgentJobLocked(ctx context.Context, job model.Job, reason string, status *runner.Status) error {
	return s.finishFailedAgentJobLocked(ctx, job, reason, status, false)
}

func (s *Server) finishFailedAgentJobLocked(ctx context.Context, job model.Job, reason string, status *runner.Status, onlyWithoutOutput bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	outputArrived, waiting, changed := false, false, false
	if err := s.DB.Update(func(st *model.State) error {
		j := st.Jobs[job.ID]
		if j.Status != "running" || !sameManagedRun(j, job) {
			return nil
		}
		// Commit failure atomically with the output handler's phase transition.
		if onlyWithoutOutput && j.Run.AbortReason == "" && (j.Run.Phase == model.PhaseOutputReceived || j.Run.Phase == model.PhaseFinalizing) {
			job, outputArrived = j, true
			return nil
		}
		revokeJobToken(st, j.ID)
		stopped := status != nil && (status.State == runner.StateStopped || status.State == runner.StateNone && !time.Now().Before(j.Run.PreparedAt.Add(5*time.Minute)))
		expired := j.Run != nil && time.Now().After(j.Run.Deadline.Add(10*time.Minute))
		if j.Run != nil && !stopped && !expired {
			if j.Run.AbortReason == "" {
				j.Run.AbortReason = reason
			}
			waiting = true
		} else {
			j.Status, j.Error = "failed", reason
			if j.Run != nil {
				if j.Run.AbortReason != "" {
					j.Error = j.Run.AbortReason
				}
				j.Run.CostCents = nil
				if stopped {
					j.Run.CostCents = status.Cost
				}
			}
		}
		job, changed = j, true
		st.Jobs[j.ID] = j
		return nil
	}); err != nil {
		return err
	}
	if outputArrived {
		return s.finalizeAgentJobLocked(ctx, job, *status)
	}
	if !changed || job.Run == nil {
		return nil
	}
	if waiting {
		if job.Run.Ref.ID != "" {
			r, err := s.runners.New(job)
			if err == nil {
				err = r.Stop(ctx, attempt(job))
			}
			if err != nil {
				slog.Warn("agent interrupt failed", "job", job.ID, "error", err)
			}
		}
		return fmt.Errorf("%w: waiting for agent session to stop", runner.ErrTemporary)
	}
	return s.cleanupAgentJob(ctx, job, status)
}

// abortAgentJob is called while the deletion handler holds generate:<project>.
func (s *Server) abortAgentJob(ctx context.Context, job model.Job) error {
	if _, err := s.runners.New(job); err != nil {
		return fail(503, "managed agents unavailable; retry deletion after restoring configuration")
	}
	if err := s.failAgentJobLocked(ctx, job, "generation aborted by deletion", nil); err != nil && !errors.Is(err, runner.ErrTemporary) {
		return err
	}
	return fail(503, "agent shutdown pending; retry deletion")
}

func (s *Server) abortProjectAgentJobs(ctx context.Context, pid string) error {
	var jobs []model.Job
	if err := s.DB.View(func(st model.State) error {
		for _, j := range st.Jobs {
			if j.ProjectID == pid && j.Run != nil && (j.Status == "running" || !j.Run.CleanupComplete) {
				jobs = append(jobs, j)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Status == "running" {
			return s.abortAgentJob(ctx, j)
		}
		if _, err := s.runners.New(j); err != nil {
			return fail(503, "managed agents unavailable; retry deletion after restoring configuration")
		}
		if err := s.cleanupAgentJob(ctx, j, nil); err != nil {
			return fail(503, "agent cleanup pending; retry deletion")
		}
		complete := false
		if err := s.DB.View(func(st model.State) error {
			complete = st.Jobs[j.ID].Run != nil && st.Jobs[j.ID].Run.CleanupComplete
			return nil
		}); err != nil {
			return err
		}
		if !complete {
			return fail(503, "agent cleanup pending; retry deletion")
		}
	}
	return nil
}

func (s *Server) cleanupAgentJob(ctx context.Context, job model.Job, status *runner.Status) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var current model.Job
	if err := s.DB.View(func(st model.State) error { current = st.Jobs[job.ID]; return nil }); err != nil {
		return err
	}
	if !sameManagedRun(current, job) || current.Run == nil || current.Run.CleanupComplete || current.Status != "completed" && current.Status != "failed" {
		return nil
	}
	job = current
	r, err := s.runners.New(job)
	if err != nil {
		return err
	}
	if status != nil && (status.State == runner.StateRunning || status.State == runner.StateUnknown) {
		return r.Stop(cleanupCtx, attempt(job))
	}
	complete := true
	if err := r.Cleanup(cleanupCtx, attempt(job)); err != nil {
		complete = false
		slog.Warn("agent resource cleanup deferred", "job", job.ID, "error", err)
	}
	unlock, err := s.lockProject(cleanupCtx, job.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	keys, err := s.Blobs.List(cleanupCtx, agentJobPrefix(job))
	if err != nil {
		return err
	}
	for _, key := range keys {
		if strings.HasSuffix(string(key), ".tar.gz") {
			if err := s.Blobs.Delete(cleanupCtx, key); err != nil {
				complete = false
				slog.Warn("agent archive cleanup failed", "job", job.ID, "error", err)
			}
		}
	}
	if complete {
		return s.DB.Update(func(st *model.State) error {
			j := st.Jobs[job.ID]
			if sameManagedRun(j, job) && (j.Status == "completed" || j.Status == "failed") {
				j.Run.CleanupComplete = true
				st.Jobs[j.ID] = j
			}
			return nil
		})
	}
	return nil
}
