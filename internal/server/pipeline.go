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
	if fatal := s.fatalError.Load(); fatal != nil {
		return fatal
	}
	if err := s.Prepare(); err != nil {
		return err
	}
	if err := s.reconcileAgentJobs(ctx); err != nil {
		s.reportFatal(s.databaseError(ctx, err))
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
		case <-s.fatal:
			return s.fatalError.Load()
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-ticker.C:
		}
		if err := s.runQueuedJobs(ctx); err != nil {
			s.reportFatal(s.databaseError(ctx, err))
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
			var fatal *store.FatalDatabaseError
			var unknown *store.UnknownCommit
			if errors.As(err, &fatal) || errors.As(err, &unknown) {
				return err
			}
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
		jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err = s.startAgentJob(jobCtx, job, deferredStarts...)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			var fatal *store.FatalDatabaseError
			var unknown *store.UnknownCommit
			if errors.As(err, &fatal) || errors.As(err, &unknown) {
				return err
			}
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
				s.reportFatal(s.databaseError(ctx, err))
				slog.Error("agent reconciliation deferred", "error", err)
			}
			lastReconcile = now
		}
		if err := s.scheduleGenerations(now); err != nil {
			s.reportFatal(s.databaseError(ctx, err))
			slog.Error("generation scheduling deferred", "error", err)
		}
		if now.Sub(lastGC) >= time.Hour {
			if _, err := s.storage.collectAllGarbage(ctx); err != nil {
				s.reportFatal(s.databaseError(ctx, err))
				slog.Warn("garbage collection deferred", "error", err)
			}
			lastGC = now
		}
		select {
		case <-s.fatal:
			return
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
		if j.ProjectID != pid || j.ParentID != nil {
			return fail(404, "job not found")
		}
		job, err = jobProjection(q, j)
		return err
	}); err != nil {
		return err
	}
	jsonOut(w, 200, job)
	return nil
}
