package server

import (
	"context"
	"log/slog"
)

// ReconcileJob handles a runner exit notification using the caller's lifetime context.
func (s *Server) ReconcileJob(ctx context.Context, jobID, attemptID string) {
	if ctx.Err() != nil {
		return
	}
	job, err := s.loadJob(ctx, jobID)
	if err != nil {
		s.reportFatal(s.databaseError(ctx, err))
		return
	}
	if job.Status != "running" || job.Data.Run == nil || job.ID != attemptID {
		return
	}
	if err := s.reconcileAgentJob(ctx, job); err != nil && ctx.Err() == nil {
		s.reportFatal(s.databaseError(ctx, err))
		slog.Error("agent reconciliation deferred", "job", jobID, "error", err)
	}
}
