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
			var job model.Job
			err := s.DB.Update(func(st *model.State) error {
				pending := []model.Job{}
				running := map[string]bool{}
				sessions := 0
				for _, j := range st.Jobs {
					if j.Status == "running" {
						running[j.ProjectID] = true
						if j.Run != nil {
							sessions++
						}
					}
				}
				for _, j := range st.Jobs {
					if j.Status == "queued" {
						if running[j.ProjectID] || sessions >= s.Config.AgentMaxSessions {
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
	if err := s.DB.View(func(st model.State) error {
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
		j, err = s.DB.ArchivedJob(r.Context(), pid, id)
		if err != nil {
			return fail(404, "job not found")
		}
	}
	if j.ProjectID != pid || j.Created.Before(validAfter) {
		return fail(404, "job not found")
	}
	jsonOut(w, 200, j)
	return nil
}
func (s *Server) retry(w http.ResponseWriter, r *http.Request, uid string) error {
	pid, id := r.PathValue("project"), r.PathValue("job")
	if err := s.check(pid, uid); err != nil {
		return err
	}
	var archived model.Job
	var found bool
	var cutoff time.Time
	if err := s.DB.View(func(st model.State) error {
		_, found = st.Jobs[id]
		cutoff = st.Projects[pid].JobsValidAfter
		return nil
	}); err != nil {
		return err
	}
	if !found {
		var err error
		archived, err = s.DB.ArchivedJob(r.Context(), pid, id)
		if err != nil || archived.Created.Before(cutoff) {
			return fail(404, "job not found")
		}
	}
	_, principal := s.session(r)
	var job model.Job
	err := s.DB.Update(func(st *model.State) error {
		pid := r.PathValue("project")
		if _, err := member(*st, pid, uid); err != nil {
			return err
		}
		j, ok := st.Jobs[r.PathValue("job")]
		if !ok && archived.ID != "" {
			j = archived
			ok = true
		}
		if !ok || j.ProjectID != pid || j.Created.Before(st.Projects[pid].JobsValidAfter) {
			return fail(404, "job not found")
		}
		if j.AnthropicConnectionID != "" {
			return fail(409, "BYOK has been removed; create a new generation job")
		}
		if j.Status != "failed" {
			return fail(409, "only failed jobs can be retried")
		}
		if j.Run != nil && !j.Run.CleanupComplete {
			return fail(503, "agent cleanup pending; retry after reconciliation")
		}

		queued := 0
		for _, v := range st.Jobs {
			if v.ProjectID == pid && (v.Status == "queued" || v.Status == "running") {
				queued++
			}
		}
		if queued >= s.Config.MaxQueued {
			return fail(429, "project job queue is full")
		}
		j.Status = "queued"
		j.Run = nil
		j.Unchanged = false
		j.Error = ""
		j.UserID = uid
		j.AgentID = principal.AgentID
		st.Jobs[j.ID] = j
		job = j
		return nil
	})
	if err != nil {
		return err
	}
	s.notify()
	jsonOut(w, 202, job)
	return nil
}
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request, uid string) error {
	if r.URL.Query().Get("manifest") == "1" || r.URL.Query().Get("file") != "" {
		return s.syncSnapshot(w, r, uid)
	}
	var rev model.Revision
	err := s.DB.View(func(st model.State) error {
		pid := r.PathValue("project")
		p, err := member(st, pid, uid)
		if err != nil {
			return err
		}
		rid := p.Revision

		if rid == "" {
			return fail(409, "project has no wiki yet")
		}
		var ok bool
		rev, ok = st.Revisions[rid]
		if !ok || rev.ProjectID != pid {
			return fail(404, "revision not found")
		}
		return nil
	})
	if err != nil {
		return err
	}
	files := map[string]string{}
	for name, key := range rev.Files {
		b, err := s.Blobs.Get(r.Context(), key)
		if err != nil {
			return err
		}
		files[name] = string(b)
	}
	if requested := r.URL.Query().Get("revision"); requested != "" && requested != snapshotToken(files) {
		return fail(404, "wiki history is not retained")
	}
	jsonOut(w, 200, model.Snapshot{Revision: snapshotToken(files), Files: files})
	return nil
}
