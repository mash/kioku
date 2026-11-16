package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func (s *Server) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Server) RunWorker(ctx context.Context) error {
	if err := s.Prepare(); err != nil {
		return err
	}
	if err := s.reconcileAgentJobs(ctx); err != nil {
		slog.Error("agent reconciliation deferred", "error", err)
	}
	maintenanceCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.runMaintenance(maintenanceCtx) }()
	defer func() { cancel(); <-done }()
	s.notify()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-ticker.C:
		}
		if err := s.runQueuedJobs(ctx); err != nil {
			slog.Error("generation work deferred", "error", err)
		}
	}
}

// One wakeup drains eligible work, but never retries a failed job in the same
// pass. Notifications received during that pass coalesce into its failure.
func (s *Server) runQueuedJobs(ctx context.Context) (err error) {
	var errs []error
	defer func() {
		err = errors.Join(append(errs, err)...)
		if err != nil {
			select {
			case <-s.wake:
			default:
			}
		}
	}()
	deferredViews := map[string]bool{}
	var deferredStarts []string
	for ctx.Err() == nil {
		if err = s.advanceViewGenerations(ctx, deferredViews); err != nil {
			errs = append(errs, err)
		}
		var job store.JobRow
		err = s.Store.Read(ctx, func(q *store.Queries) error {
			var e error
			job, e = q.NextQueued(s.Config.AgentMaxSessions, deferredStarts...)
			return e
		})
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		err = s.startAgentJob(ctx, job, deferredStarts...)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			errs = append(errs, err)
			deferredStarts = append(deferredStarts, job.ID)
		}
	}
	return nil
}

func (s *Server) runMaintenance(ctx context.Context) {
	var lastReconcile, lastGC time.Time
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		if now.Sub(lastReconcile) >= 30*time.Second {
			if err := s.reconcileAgentJobs(ctx); err != nil {
				slog.Error("agent reconciliation deferred", "error", err)
			}
			lastReconcile = now
		}
		if err := s.scheduleGenerations(now); err != nil {
			slog.Error("generation scheduling deferred", "error", err)
		}
		if now.Sub(lastGC) >= time.Hour {
			if err := s.storage.pruneExpired(ctx); err != nil {
				slog.Error("expired row pruning deferred", "error", err)
			}
			if _, err := s.storage.collectAllGarbage(ctx); err != nil {
				slog.Warn("garbage collection deferred", "error", err)
			}
			lastGC = now
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) job(w http.ResponseWriter, r *http.Request, uid string) error {
	pid, id := r.PathValue("project"), r.PathValue("job")
	var job model.Job
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		if _, err := sqlMember(q, pid, uid); err != nil {
			return err
		}
		j, err := q.Job(id)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "job not found")
		}
		if err != nil {
			return err
		}
		if j.ProjectID != pid || j.ParentID != nil || j.Kind == "view" {
			return fail(404, "job not found")
		}
		job = jobValue(j)
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 200, job)
	return nil
}
