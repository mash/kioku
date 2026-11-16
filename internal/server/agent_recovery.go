package server

import (
	"context"
	"log/slog"
)

// ReconcileJob handles a runner exit notification using the caller's lifetime context.
func (s *Server) ReconcileJob(ctx context.Context, jobID string) {
	if ctx.Err() != nil {
		return
	}
	job, err := s.loadJob(ctx, jobID)
	if err != nil {
		slog.Error("agent reconciliation deferred", "job", jobID, "error", err)
		return
	}
	if !job.Running() {
		return
	}
	if err := s.reconcileAgentJob(ctx, job); err != nil && ctx.Err() == nil {
		slog.Error("agent reconciliation deferred", "job", jobID, "error", err)
	}
}
