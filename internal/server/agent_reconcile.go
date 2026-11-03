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
	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/wiki"
)

func (s *Server) reconcileAgentJobs(ctx context.Context) error {
	var jobs []model.Job
	if err := s.DB.View(func(st model.State) error {
		for _, j := range st.Jobs {
			if j.Managed != nil && (j.Status == "running" || ((j.Status == "completed" || j.Status == "failed") && !j.Managed.CleanupComplete)) {
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
	if current.Managed == nil || !sameManagedRun(current, job) {
		return nil
	}
	if (current.Status == "completed" || current.Status == "failed") && !current.Managed.CleanupComplete {
		return s.cleanupAgentJob(ctx, current)
	}
	if current.Status != "running" {
		return nil
	}
	job = current
	run := job.Managed
	now := time.Now()
	if !s.hasManagedClient(job) {
		if now.After(run.Deadline) {
			return s.failAgentJobLocked(ctx, job, "managed agents unavailable; job timed out", false)
		}
		return nil
	}
	if run.AbortReason != "" {
		return s.failAgentJobLocked(ctx, job, run.AbortReason, true)
	}
	// Output can arrive before the session ID is committed. Both phases need
	// the same recovery for a lost session-create response or process crash.
	if run.SessionID == "" {
		if now.Before(run.PreparedAt.Add(5*time.Minute)) && now.Before(run.Deadline) {
			return nil
		}
		id, findErr := s.findAgentSession(ctx, job)
		if findErr != nil && errors.Is(findErr, managedagents.ErrTemporary) && now.Before(run.Deadline) {
			return nil
		}
		if id != "" {
			if err = s.DB.Update(func(st *model.State) error {
				j := st.Jobs[job.ID]
				if !sameManagedRun(j, job) || j.Status != "running" {
					return nil
				}
				j.Managed.SessionID = id
				st.Jobs[j.ID] = j
				return nil
			}); err != nil {
				return err
			}
			job.Managed.SessionID = id
		}
		return s.failAgentJobLocked(ctx, job, "session creation did not finish", true)
	}
	if run.Phase == model.PhaseFinalizing {
		return s.finalizeAgentJobLocked(ctx, job)
	}
	if now.After(run.Deadline) {
		return s.failAgentJobLocked(ctx, job, "agent job timed out", true)
	}
	session, err := s.managedClient(job).GetSession(ctx, run.SessionID)
	if err != nil {
		if errors.Is(err, managedagents.ErrTemporary) {
			return nil
		}
		return s.failAgentJobLocked(ctx, job, "could not inspect agent session", true)
	}
	switch session.Status {
	case "idle":
		switch session.LastStopReason {
		case "end_turn", "budget_reached":
			if run.Phase == model.PhaseOutputReceived {
				return s.finalizeAgentJobLocked(ctx, job)
			}
			if run.Phase == model.PhaseAgentRunning {
				return s.finishFailedAgentJobLocked(ctx, job, "agent stopped without uploading output", false, true, session)
			}
		case "retries_exhausted":
			return s.failAgentJobLocked(ctx, job, "agent gave up after errors", false, session)
		case "requires_action":
			return s.failAgentJobLocked(ctx, job, "agent requires action", true, session)
		}
	case "terminated":
		if run.Phase == model.PhaseOutputReceived {
			return s.finalizeAgentJobLocked(ctx, job)
		}
		if run.Phase == model.PhaseAgentRunning {
			return s.finishFailedAgentJobLocked(ctx, job, "session terminated", false, true, session)
		}
	}
	return nil
}
func (s *Server) finalizeAgentJob(ctx context.Context, job model.Job) error {
	unlock, err := s.lockProject(ctx, "generate:"+job.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.finalizeAgentJobLocked(ctx, job)
}
func (s *Server) finalizeAgentJobLocked(ctx context.Context, job model.Job) error {
	proceed := false
	if err := s.DB.Update(func(st *model.State) error {
		j := st.Jobs[job.ID]
		if j.Status != "running" || j.Managed == nil || !sameManagedRun(j, job) {
			return nil
		}
		if j.Managed.Phase != model.PhaseOutputReceived && j.Managed.Phase != model.PhaseFinalizing {
			return nil
		}
		j.Managed.Phase = model.PhaseFinalizing
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
	failJob := func(reason string, observed ...managedagents.Session) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.failAgentJobLocked(ctx, job, reason, false, observed...)
	}
	body, err := s.Blobs.Get(ctx, job.Managed.OutputKey)
	if err != nil {
		return failJob("could not read agent output")
	}
	if auth.Hash(string(body)) != job.Managed.OutputHash {
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
	session, err := s.managedClient(job).GetSession(ctx, job.Managed.SessionID)
	if err != nil {
		if errors.Is(err, managedagents.ErrTemporary) && time.Now().Before(job.Managed.Deadline) {
			return nil
		}
		return failJob("final agent usage unavailable")
	}
	if session.Status == "running" || session.Status == "rescheduling" {
		if time.Now().After(job.Managed.Deadline) {
			return s.failAgentJobLocked(ctx, job, "agent job timed out", true)
		}
		return nil
	}
	var project model.Project
	if err = s.DB.View(func(st model.State) error { project = st.Projects[job.ProjectID]; return nil }); err != nil {
		return err
	}
	project.SourceVersion = job.Managed.SourceVersion
	project.NextIntakeSequence = job.Managed.NextIntakeSequence
	project.SourcesUpdatedAt = job.Managed.SourcesUpdatedAt
	sources := make([]model.Source, 0, len(inputs))
	for _, in := range inputs {
		sources = append(sources, in.Source)
	}
	_, err = s.publishWiki(ctx, publishInput{Job: job, Project: project, Sources: sources, Files: files, Fingerprint: agentFingerprint(inputs, job.Managed), Validate: func(st model.State) error { return validateGeneration(st, job, sources) }, Finish: func(st *model.State, now time.Time) error {
		revokeGeneratorToken(st, job.ID)
		j := st.Jobs[job.ID]
		j.Managed.ListCostCents = session.ListCostCents
		st.Jobs[j.ID] = j
		return nil
	}})
	if err != nil {
		return failJob(err.Error(), session)
	}
	return s.cleanupAgentJob(ctx, job)
}
func agentFingerprint(inputs []wiki.Input, run *model.ManagedRun) string {
	type item struct{ ID, Hash string }
	v := struct {
		Version, MAAgentID string
		MAAgentVersion     int64
		Sources            []item
	}{Version: "agent-v1", MAAgentID: run.MAAgentID, MAAgentVersion: run.MAAgentVersion}
	for _, in := range inputs {
		v.Sources = append(v.Sources, item{in.Source.ID, in.Source.ContentHash})
	}
	b, _ := json.Marshal(v)
	return auth.Hash(string(b))
}
func (s *Server) failAgentJob(ctx context.Context, job model.Job, reason string) error {
	unlock, err := s.lockProject(ctx, "generate:"+job.ProjectID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.failAgentJobLocked(ctx, job, reason, false)
}
func (s *Server) failAgentJobLocked(ctx context.Context, job model.Job, reason string, interrupt bool, observed ...managedagents.Session) error {
	return s.finishFailedAgentJobLocked(ctx, job, reason, interrupt, false, observed...)
}
func (s *Server) finishFailedAgentJobLocked(ctx context.Context, job model.Job, reason string, interrupt, onlyWithoutOutput bool, observed ...managedagents.Session) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var current model.Job
	if err := s.DB.View(func(st model.State) error { current = st.Jobs[job.ID]; return nil }); err != nil {
		return err
	}
	if current.ID == "" || current.Status != "running" || !sameManagedRun(current, job) {
		return nil
	}
	job = current
	known := false
	live := false
	var cents int64
	// Reconciliation already observed a stopped session. A transient failure
	// on a second lookup must not discard that known terminal usage.
	if len(observed) == 1 && job.Managed != nil && observed[0].ID == job.Managed.SessionID && (observed[0].Status == "idle" || observed[0].Status == "terminated") {
		known = true
		cents = observed[0].ListCostCents
	}
	if job.Managed != nil && job.Managed.SessionID != "" && s.hasManagedClient(job) {
		if interrupt {
			if err := s.managedClient(job).Interrupt(ctx, job.Managed.SessionID); err != nil {
				slog.Warn("agent interrupt failed", "job", job.ID, "error", err)
			}
		}
		if session, err := s.managedClient(job).GetSession(ctx, job.Managed.SessionID); err == nil {
			known = true
			cents = session.ListCostCents
			live = session.Status == "running" || session.Status == "rescheduling"
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	outputArrived := false
	waitingForStop := false
	if err := s.DB.Update(func(st *model.State) error {
		j := st.Jobs[job.ID]
		if j.Status != "running" || !sameManagedRun(j, job) {
			return nil
		}
		// The output handler can commit while session inspection is in flight.
		// Check and commit failure atomically with its output phase transition.
		if onlyWithoutOutput && j.Managed != nil && j.Managed.Phase == model.PhaseOutputReceived {
			job = j
			outputArrived = true
			return nil
		}
		if live {
			// An accepted interrupt can also be asynchronous. Preserve the durable
			// stop intent until an observed terminal cost can be recorded.
			revokeGeneratorToken(st, j.ID)
			j.Managed.AbortReason = reason
			st.Jobs[j.ID] = j
			waitingForStop = true
			return nil
		}
		revokeGeneratorToken(st, j.ID)
		j.Status = "failed"
		j.Error = reason
		if j.Managed != nil {
			j.Managed.ListCostCents = cents
			j.Managed.StopPending = !known && (j.Managed.SessionID != "" || interrupt && j.Managed.VaultID != "")
		}
		job = j
		st.Jobs[j.ID] = j
		return nil
	}); err != nil {
		return err
	}
	if outputArrived {
		return s.finalizeAgentJobLocked(ctx, job)
	}
	if waitingForStop {
		return fmt.Errorf("%w: waiting for agent session to stop", managedagents.ErrTemporary)
	}
	return s.cleanupAgentJob(ctx, job)
}

// abortAgentJob is called while the deletion handler holds generate:<project>.
func (s *Server) abortAgentJob(ctx context.Context, job model.Job) error {
	if job.Managed != nil && !s.hasManagedClient(job) {
		return fail(503, "managed agents unavailable; retry deletion after restoring configuration")
	}
	if job.Managed != nil && job.Managed.SessionID == "" && s.hasManagedClient(job) {
		id, err := s.findAgentSession(ctx, job)
		if err != nil {
			return fail(503, "could not locate agent session; retry deletion")
		}
		if id != "" {
			if err = s.DB.Update(func(st *model.State) error {
				j := st.Jobs[job.ID]
				if j.Status != "running" || !sameManagedRun(j, job) {
					return fail(409, "generation changed")
				}
				j.Managed.SessionID = id
				st.Jobs[j.ID] = j
				job = j
				return nil
			}); err != nil {
				return err
			}
		}
	}

	if err := s.failAgentJobLocked(ctx, job, "generation aborted by deletion", true); err != nil {
		if errors.Is(err, managedagents.ErrTemporary) {
			return fail(503, "agent shutdown pending; retry deletion")
		}
		return err
	}
	return nil
}
func (s *Server) abortProjectAgentJobs(ctx context.Context, pid string) error {
	var jobs []model.Job
	if err := s.DB.View(func(st model.State) error {
		for _, j := range st.Jobs {
			if j.ProjectID == pid && j.Managed != nil && (j.Status == "running" || !j.Managed.CleanupComplete) {
				jobs = append(jobs, j)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, j := range jobs {
		var err error
		if j.Status == "running" {
			err = s.abortAgentJob(ctx, j)
		} else {
			err = s.cleanupAgentJob(ctx, j)
		}
		if err != nil {
			return err
		}
		complete := false
		if err = s.DB.View(func(st model.State) error {
			current := st.Jobs[j.ID]
			complete = current.Managed != nil && current.Managed.CleanupComplete
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
func (s *Server) cleanupAgentJob(ctx context.Context, job model.Job) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// Callers may still hold the pre-publication snapshot. Always use the
	// durable cleanup and stop intent belonging to this exact attempt.
	var current model.Job
	if err := s.DB.View(func(st model.State) error { current = st.Jobs[job.ID]; return nil }); err != nil {
		return err
	}
	if !sameManagedRun(current, job) || current.Status != "completed" && current.Status != "failed" {
		return nil
	}
	job = current
	if job.Managed != nil && job.Managed.StopPending {
		if !s.stopUnknownAgent(cleanupCtx, &job) {
			return nil
		}
	}
	complete := true
	if job.Managed != nil {
		if !s.hasManagedClient(job) {
			complete = false
		}
		if s.hasManagedClient(job) {
			if id := job.Managed.SessionID; id != "" {
				if err := s.managedClient(job).ArchiveSession(cleanupCtx, id); err != nil {
					complete = false
					slog.Warn("agent session archive failed", "job", job.ID, "session", id, "error", err)
				}
			}
			vaults := []string{job.Managed.VaultID}
			if job.Managed.VaultID == "" {
				var err error
				vaults, err = s.managedClient(job).FindJobVaults(cleanupCtx, job.ID, job.Managed.Attempt.ID)
				if err != nil {
					complete = false
					slog.Warn("agent vault discovery failed", "job", job.ID, "error", err)
				}
			}
			for _, id := range vaults {
				if err := s.managedClient(job).ArchiveVault(cleanupCtx, id); err != nil {
					complete = false
					slog.Warn("agent vault archive failed", "job", job.ID, "vault", id, "error", err)
				}
			}
		}
	}
	unlock, err := s.lockProject(cleanupCtx, job.ProjectID)
	if err != nil {
		slog.Warn("agent archive cleanup deferred", "job", job.ID, "error", err)
		return nil
	}
	defer unlock()
	keys, err := s.Blobs.List(cleanupCtx, agentJobPrefix(job))
	if err != nil {
		slog.Warn("agent archive listing failed", "job", job.ID, "error", err)
		return nil
	}
	for _, key := range keys {
		if strings.HasSuffix(key, ".tar.gz") {
			if err := s.Blobs.Delete(cleanupCtx, key); err != nil {
				complete = false
				slog.Warn("agent archive cleanup failed", "job", job.ID, "error", err)
			}
		}
	}
	if complete && job.Managed != nil {
		return s.DB.Update(func(st *model.State) error {
			j := st.Jobs[job.ID]
			if sameManagedRun(j, job) && (j.Status == "completed" || j.Status == "failed") {
				j.Managed.CleanupComplete = true
				st.Jobs[j.ID] = j
			}
			return nil
		})
	}
	return nil
}

// A job that failed with unknown usage must still stop a possibly running
// provider session. Its observed later cost is audit-only.
func (s *Server) stopUnknownAgent(ctx context.Context, job *model.Job) bool {
	if !s.hasManagedClient(*job) {
		return false
	}
	id := job.Managed.SessionID
	if id == "" {
		var err error
		id, err = s.findAgentSession(ctx, *job)
		if err != nil || id == "" && time.Now().Before(job.Managed.PreparedAt.Add(5*time.Minute)) {
			return false
		}
	}
	cents := job.Managed.ListCostCents
	if id != "" {
		if err := s.managedClient(*job).Interrupt(ctx, id); err != nil {
			slog.Warn("unknown-cost session interrupt deferred", "job", job.ID, "error", err)
		}
		session, err := s.managedClient(*job).GetSession(ctx, id)
		// GetSession can return a verified status with an error for unavailable
		// usage or events. Stopping and reclaiming the slot do not require cost;
		// the failed job already retained unknown usage without charging it.
		if session.Status != "idle" && session.Status != "terminated" {
			return false
		}
		if err == nil {
			cents = session.ListCostCents
		}
	}
	if err := s.DB.Update(func(st *model.State) error {
		j := st.Jobs[job.ID]
		if !sameManagedRun(j, *job) || j.Status != "failed" {
			return errors.New("agent cleanup attempt changed")
		}
		j.Managed.SessionID = id
		j.Managed.ListCostCents = cents
		j.Managed.StopPending = false
		st.Jobs[j.ID] = j
		*job = j
		return nil
	}); err != nil {
		slog.Warn("unknown-cost session cleanup deferred", "job", job.ID, "error", err)
		return false
	}
	return true
}

// A retry reuses the job ID. A lost archive from an older attempt must never
// supply a session or usage for the new attempt.
func (s *Server) findAgentSession(ctx context.Context, job model.Job) (string, error) {
	// Session-list metadata identifies the attempt without depending on the
	// detail/cost endpoint, which may be unavailable while interrupt works.
	return s.managedClient(job).FindSessionByJob(ctx, job.ID, job.Managed.Attempt.ID)
}
