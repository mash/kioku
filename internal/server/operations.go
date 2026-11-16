package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
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

// maxRateHosts bounds the memory of the rate limiter.
const maxRateHosts = 10000

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
	} else if r.URL.Path == "/oauth/register" {
		// Registration is anonymous and stores a row; PruneSessions deletes
		// unused clients after a day, so this bounds the rows per address.
		host += "/register"
		limit = 10
	}
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if s.rates == nil {
		s.rates = map[string]rateWindow{}
	}
	now := time.Now()
	if _, ok := s.rates[host]; !ok && len(s.rates) >= maxRateHosts {
		// A full table must not reject every client. Drop the expired windows,
		// or else the oldest window, to make room for the new host.
		oldest := ""
		for k, v := range s.rates {
			if now.Sub(v.Start) > time.Minute {
				delete(s.rates, k)
			} else if oldest == "" || v.Start.Before(s.rates[oldest].Start) {
				oldest = k
			}
		}
		if len(s.rates) >= maxRateHosts {
			delete(s.rates, oldest)
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
	err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		var err error
		projects, queued, running, failed, err = q.Metrics()
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "kioku_http_requests_total %d\nkioku_http_errors_total %d\nkioku_projects %d\nkioku_jobs_queued %d\nkioku_jobs_running %d\nkioku_jobs_failed %d\n", s.requests.Load(), s.failures.Load(), projects, queued, running, failed)
}
func (s *Server) usage(w http.ResponseWriter, r *http.Request, uid string) error {
	var result map[string]any
	err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		pid := r.PathValue("project")
		if _, err := sqlMember(q, pid, uid); err != nil {
			return err
		}
		files, err := q.ProjectWiki(pid)
		if err != nil {
			return err
		}
		size, err := wikiBytes(files)
		if err != nil {
			return err
		}
		count, pending, err := q.ProjectCounts(pid)
		if err != nil {
			return err
		}
		result = map[string]any{"wiki_bytes": size, "inbox_items": count, "pending_jobs": pending, "limits": map[string]any{"wiki_bytes": s.Config.MaxWikiBytes, "inbox_items": maxInboxItems, "pending_jobs": s.Config.MaxQueued}}
		return nil
	})
	if err != nil {
		return err
	}
	jsonOut(w, 200, result)
	return nil
}
func (s *Server) audit(w http.ResponseWriter, r *http.Request, uid string) error {
	out := []model.Audit{}
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		pid := r.PathValue("project")
		if _, err := owner(q, pid, uid); err != nil {
			return err
		}
		rows, err := q.ProjectAudit(pid)
		if err != nil {
			return err
		}
		for _, row := range rows {
			out = append(out, model.Audit{ID: row.ID, ProjectID: pid, UserID: row.Data.UserID, Action: row.Data.Action, Detail: row.Data.Detail, Created: storageDate(row.Created)})
		}
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 200, out)
	return nil
}
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	if err := s.checkDeleteOwner(pid, uid); err != nil {
		return err
	}
	unlock, err := s.storage.lock(r.Context(), "generate:"+pid)
	if err != nil {
		return err
	}
	defer unlock()
	err = s.storage.deleteProject(r.Context(), pid, func(ctx context.Context, j store.JobRow) error {
		err := s.reconcileAgentJobLocked(ctx, j)
		if errors.Is(err, runner.ErrTemporary) {
			return fail(503, "agent shutdown pending; retry deletion")
		}
		return err
	}, func(q *store.Queries) error {
		if err := s.checkMutation(q, r, uid, ""); err != nil {
			return err
		}
		role, err := q.ProjectRole(pid, uid)
		if errors.Is(err, sql.ErrNoRows) || err == nil && role != "owner" {
			return fail(404, "project not found")
		}
		return err
	})
	if err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"deleted": true})
	return nil
}
