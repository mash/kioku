package server

import (
	"context"
	"log/slog"

	"github.com/mash/kioku/internal/model"
)

// ReconcileJob handles a runner exit notification using the caller's lifetime context.
func (s *Server) ReconcileJob(ctx context.Context, jobID, attemptID string) {
	if ctx.Err() != nil {
		return
	}
	var job model.Job
	if err := s.DB.View(func(st model.State) error { job = st.Jobs[jobID]; return nil }); err != nil {
		return
	}
	if job.Status != "running" || job.Run == nil || job.Run.Attempt.ID != attemptID {
		return
	}
	if err := s.reconcileAgentJob(ctx, job); err != nil && ctx.Err() == nil {
		slog.Error("agent reconciliation deferred", "job", jobID, "error", err)
	}
}
