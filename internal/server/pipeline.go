package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

func (s *Server) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// RunWorker uses a durable queue. One worker serializes publication in this
// queue, so concurrent submissions cannot overwrite each other.
func (s *Server) RunWorker(ctx context.Context) error {
	if err := s.Prepare(); err != nil {
		return err
	}
	// Recover existing runs before dequeuing any new job.
	if err := s.reconcileAgentJobs(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	maintenanceCtx, stopMaintenance := context.WithCancel(ctx)
	maintenanceDone := make(chan struct{})
	go func() { defer close(maintenanceDone); s.runMaintenance(maintenanceCtx) }()
	defer func() { stopMaintenance(); <-maintenanceDone }()
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
		for ctx.Err() == nil {
			if err := s.advanceViewGenerations(ctx); err != nil {
				slog.Error("view generation deferred", "error", err)
			}
			var job model.Job
			err := s.Store.Update(func(st *model.State) error {
				pending := []model.Job{}
				running := map[string]bool{}
				sessions := 0
				for _, j := range st.Jobs {
					if j.Status == "running" {
						running[j.ProjectID] = true
						if j.ViewGeneration == nil {
							sessions++
						}
					}
				}
				for _, j := range st.Jobs {
					if j.Status == "queued" {
						if sessions >= s.Config.AgentMaxSessions || (running[j.ProjectID] && j.ViewTask == nil) {
							continue
						}
						pending = append(pending, j)
					}
				}
				if len(pending) == 0 {
					return nil
				}
				sort.Slice(pending, func(i, j int) bool {
					if pending[i].Created.Equal(pending[j].Created) {
						return pending[i].ID < pending[j].ID
					}
					return pending[i].Created.Before(pending[j].Created)
				})
				job = pending[0]
				job.Status = "running"
				if job.WikiStatus != "" {
					job.WikiStatus = "running"
				}
				st.Jobs[job.ID] = job
				return nil
			})
			if err != nil {
				return err
			}
			if job.ID == "" {
				break
			}
			jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			err = s.startAgentJob(jobCtx, job)
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, runner.ErrTemporary) {
				slog.Warn("agent start awaits reconciliation", "job", job.ID)
			} else if err != nil {
				return err
			}
		}
	}
}

// Maintenance must keep running while the generation queue is busy.
func (s *Server) runMaintenance(ctx context.Context) {
	count, err := s.Store.CountProjects(func(p model.Project) bool { return !p.NextGenerateAt.IsZero() })
	if err != nil {
		slog.Error("generation scheduling deferred", "error", err)
	} else {
		slog.Info("automatic generation deadlines loaded", "projects", count)
	}
	var lastReconcile time.Time
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if time.Since(lastReconcile) >= 30*time.Second {
			if err := s.reconcileAgentJobs(ctx); err != nil {
				slog.Error("agent reconciliation deferred", "error", err)
			}
			lastReconcile = time.Now()
		}
		if err := s.scheduleGenerations(time.Now().UTC()); err != nil {
			slog.Error("generation scheduling deferred", "error", err)
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
	var j model.Job
	var found bool
	var validAfter time.Time
	if err := s.Store.View(func(st model.State) error {
		p, err := member(st, pid, uid)
		if err != nil {
			return err
		}
		j, found = st.Jobs[id]
		validAfter = p.JobsValidAfter
		return nil
	}); err != nil {
		return err
	}
	if !found {
		var err error
		j, err = s.Store.ArchivedJob(r.Context(), pid, id)
		if err != nil {
			return fail(404, "job not found")
		}
	}
	if j.ProjectID != pid || j.ViewTask != nil || j.Created.Before(validAfter) {
		return fail(404, "job not found")
	}
	jsonOut(w, 200, j)
	return nil
}
