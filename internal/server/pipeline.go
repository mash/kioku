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
		for ctx.Err() == nil {
			if err := s.advanceViewGenerations(ctx); err != nil {
				s.reportFatal(s.databaseError(ctx, err))
				slog.Error("view generation deferred", "error", err)
				break
			}
			var job store.JobRow
			err := s.Store.Read(ctx, func(q *store.Queries) error {
				var err error
				job, err = q.NextQueued(s.Config.AgentMaxSessions)
				return err
			})
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				s.reportFatal(s.databaseError(ctx, err))
				slog.Error("queue selection deferred", "error", err)
				break
			}
			jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			err = s.startAgentJob(jobCtx, job)
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				s.reportFatal(s.databaseError(ctx, err))
				slog.Warn("agent start deferred", "job", job.ID, "error", err)
				break
			}
		}
	}
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
