package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

type responseLog struct {
	http.ResponseWriter
	status int
}

func (w *responseLog) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type rateWindow struct {
	Start time.Time
	Count int
}

func (s *Server) allow(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.Config.TrustedProxy != nil && s.Config.TrustedProxy.Contains(net.ParseIP(host)) {
		forwarded := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		if ip := net.ParseIP(strings.TrimSpace(forwarded[len(forwarded)-1])); ip != nil {
			host = ip.String()
		}
	}
	limit := s.Config.RatePerMinute
	if strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/api/auth/") {
		host += "/auth"
		limit = 20
	}
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if s.rates == nil {
		s.rates = map[string]rateWindow{}
	}
	now := time.Now()
	if len(s.rates) > 10000 {
		for k, v := range s.rates {
			if now.Sub(v.Start) > time.Minute {
				delete(s.rates, k)
			}
		}
		if len(s.rates) > 10000 {
			return false
		}
	}
	v := s.rates[host]
	if now.Sub(v.Start) > time.Minute {
		v = rateWindow{Start: now}
	}
	v.Count++
	s.rates[host] = v
	return v.Count <= limit
}
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.Config.MetricsToken == "" || subtle.ConstantTimeCompare([]byte(auth.Hash(token)), []byte(auth.Hash(s.Config.MetricsToken))) != 1 {
		writeError(w, fail(401, "metrics token required"))
		return
	}
	var projects, queued, running, failed int
	err := s.DB.View(func(st model.State) error {
		projects = len(st.Projects)
		for _, j := range st.Jobs {
			switch j.Status {
			case "queued":
				queued++
			case "running":
				running++
			case "failed":
				failed++
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "kioku_http_requests_total %d\nkioku_http_errors_total %d\nkioku_projects %d\nkioku_jobs_queued %d\nkioku_jobs_running %d\nkioku_jobs_failed %d\n", s.requests.Load(), s.failures.Load(), projects, queued, running, failed)
}
func (s *Server) usage(w http.ResponseWriter, r *http.Request, uid string) error {
	return s.DB.View(func(st model.State) error {
		pid := r.PathValue("project")
		if _, err := member(st, pid, uid); err != nil {
			return err
		}
		var bytes int64
		count, revisions, jobs := 0, 0, 0
		reservedCount, pendingUploads := 0, 0
		var reservedBytes int64
		for _, u := range st.Uploads {
			if u.ProjectID == pid {
				pendingUploads++
				jobs++
				for _, src := range u.Sources {
					reservedCount++
					reservedBytes += src.Bytes
				}
			}
		}
		for _, v := range st.Sources {
			if v.ProjectID == pid {
				bytes += v.Bytes
				count++
			}
		}
		for _, v := range st.Revisions {
			if v.ProjectID == pid {
				revisions++
			}
		}
		for _, j := range st.Jobs {
			if j.ProjectID == pid && (j.Status == "queued" || j.Status == "running") {
				jobs++
			}
		}
		jsonOut(w, 200, map[string]any{"source_bytes": bytes, "reserved_source_bytes": reservedBytes, "reserved_sources": reservedCount, "pending_uploads": pendingUploads, "sources": count, "revisions": revisions, "pending_jobs": jobs, "limits": map[string]any{"source_bytes": s.Config.MaxBytes, "sources": s.Config.MaxSources, "pending_jobs": s.Config.MaxQueued}})
		return nil
	})
}
func (s *Server) audit(w http.ResponseWriter, r *http.Request, uid string) error {
	return s.DB.View(func(st model.State) error {
		pid := r.PathValue("project")
		if _, err := owner(st, pid, uid); err != nil {
			return err
		}
		out := []model.Audit{}
		for _, v := range st.Audit {
			if v.ProjectID == pid {
				out = append(out, v)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
		jsonOut(w, 200, out)
		return nil
	})
}
func (s *Server) cleanup(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	if err := s.checkOwner(pid, uid); err != nil {
		return err
	}
	var req struct {
		Keep int `json:"keep"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if req.Keep < 0 || req.Keep > 1000 {
		return fail(400, "legacy keep value must be between 0 and 1000")
	}
	unlock, err := s.lockProject(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	err = s.DB.Update(func(st *model.State) error {
		p, err := owner(*st, pid, uid)
		if err != nil {
			return err
		}
		for id, current := range st.Revisions {
			if current.ProjectID == pid && id != p.Revision {
				delete(st.Revisions, id)
			}
		}
		for id, j := range st.Jobs {
			if j.ProjectID == pid && j.Status != "running" && j.Status != "queued" && (j.Run == nil || j.Run.CleanupComplete) && time.Since(j.Created) > 30*24*time.Hour {
				delete(st.Jobs, id)
			}
		}
		for id, a := range st.Audit {
			if a.ProjectID == pid && time.Since(a.Created) > 90*24*time.Hour {
				delete(st.Audit, id)
			}
		}
		for id, inv := range st.Invites {
			if inv.ProjectID == pid && inv.Expires.Before(time.Now()) {
				delete(st.Invites, id)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	removed, err := s.gc(r, pid)
	if err != nil {
		return err
	}
	jsonOut(w, 200, map[string]any{"objects_removed": removed, "history_retained": false})
	return nil
}
func (s *Server) gc(r *http.Request, pid string) (int, error) {
	live := map[blob.Key]bool{}
	if err := s.DB.View(func(st model.State) error {
		for _, src := range st.Sources {
			if src.ProjectID == pid {
				live[src.TextKey] = true
			}
		}
		for _, u := range st.Uploads {
			if u.ProjectID == pid {
				for _, src := range u.Sources {
					live[src.TextKey] = true
				}
			}
		}
		for _, rev := range st.Revisions {
			if rev.ProjectID == pid {
				for _, key := range rev.Files {
					live[key] = true
				}
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	keys, err := s.Blobs.List(r.Context(), "projects/"+pid+"/")
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, key := range keys {
		if strings.HasPrefix(string(key), "projects/"+pid+"/.kioku/") || strings.HasPrefix(string(key), "projects/"+pid+"/wiki/inbox/") {
			continue
		}
		if !live[key] {
			if err = s.Blobs.Delete(r.Context(), key); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}
func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request, uid string) error {
	pid, sid := r.PathValue("project"), r.PathValue("source")
	if err := s.checkOwner(pid, uid); err != nil {
		return err
	}
	generationUnlock, err := s.lockProject(r.Context(), "generate:"+pid)
	if err != nil {
		return err
	}
	defer generationUnlock()
	if err = s.DB.View(func(st model.State) error {
		if _, err := owner(st, pid, uid); err != nil {
			return err
		}
		src, ok := st.Sources[sid]
		if !ok || src.ProjectID != pid {
			return fail(404, "source not found")
		}
		return nil
	}); err != nil {
		return err
	}
	if err = s.abortProjectAgentJobs(r.Context(), pid); err != nil {
		return err
	}
	unlock, err := s.lockProject(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	job := model.Job{ID: auth.Token()[:20], ProjectID: pid, UserID: uid, Kind: "put", Status: "completed", Created: time.Now().UTC()}
	err = s.DB.Update(func(st *model.State) error {
		p, err := owner(*st, pid, uid)
		if err != nil {
			return err
		}
		src, ok := st.Sources[sid]
		if !ok || src.ProjectID != pid {
			return fail(404, "source not found")
		}
		delete(st.Sources, sid)
		if p.SourceHeads != nil && p.SourceHeads[src.URL] == sid {
			p.SourceHeads[src.URL] = ""
		}
		// Remove every old generated page that may contain the deleted source.
		for id, rev := range st.Revisions {
			if rev.ProjectID == pid {
				delete(st.Revisions, id)
			}
		}
		for id, j := range st.Jobs {
			if j.ProjectID == pid {
				delete(st.Jobs, id)
			}
		}
		if p.Revision != "" {
			p.WikiUpdatedAt = time.Now().UTC()
		}
		p.Revision = ""
		p.JobsValidAfter = job.Created
		p.SourceVersion++
		p.SourcesUpdatedAt = time.Now().UTC()
		job.SourceVersion = p.SourceVersion
		st.Projects[pid] = p
		st.Jobs[job.ID] = job
		recordIntake(st, job, "removed", []model.Source{src})
		return nil
	})
	if err != nil {
		return err
	}
	if _, err = s.gc(r, pid); err != nil {
		return err
	}
	jsonOut(w, 202, job)
	return nil
}
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	if err := s.checkDeleteOwner(pid, uid); err != nil {
		return err
	}
	generationUnlock, err := s.lockProject(r.Context(), "generate:"+pid)
	if err != nil {
		return err
	}
	defer generationUnlock()
	if err = s.abortProjectAgentJobs(r.Context(), pid); err != nil {
		return err
	}
	unlock, err := s.lockProject(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	err = s.DB.Update(func(st *model.State) error {
		p, ok := st.Projects[pid]
		if !ok || p.Members[uid] != "owner" {
			return fail(404, "project not found")
		}
		p.Deleting = true
		st.Projects[pid] = p
		return nil
	})
	if err != nil {
		return err
	}
	keys, err := s.Blobs.List(r.Context(), "projects/"+pid+"/")
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err = s.Blobs.Delete(r.Context(), key); err != nil {
			return err
		}
	}
	if manager, ok := s.Blobs.(interface {
		DeleteProject(context.Context, string) error
	}); ok {
		if err = manager.DeleteProject(r.Context(), pid); err != nil {
			return err
		}
	}
	err = s.DB.Update(func(st *model.State) error {
		delete(st.Projects, pid)
		for id, u := range st.Uploads {
			if u.ProjectID == pid {
				delete(st.Uploads, id)
			}
		}
		for id, v := range st.Sources {
			if v.ProjectID == pid {
				delete(st.Sources, id)
			}
		}
		for id, v := range st.Revisions {
			if v.ProjectID == pid {
				delete(st.Revisions, id)
			}
		}
		for id, v := range st.Jobs {
			if v.ProjectID == pid {
				delete(st.Jobs, id)
			}
		}
		for id, e := range st.Intake {
			if e.ProjectID == pid {
				delete(st.Intake, id)
			}
		}
		for id, v := range st.Invites {
			if v.ProjectID == pid {
				delete(st.Invites, id)
			}
		}
		for id, v := range st.Audit {
			if v.ProjectID == pid {
				delete(st.Audit, id)
			}
		}
		return nil
	})
	if err == nil {
		jsonOut(w, 200, map[string]bool{"deleted": true})
	}
	return err
}
