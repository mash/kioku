package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/mash/kioku/internal/runner/claudemanaged"
	"github.com/mash/kioku/internal/store"
)

func (s *Server) anthropicWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Config.AnthropicWebhookSigningKey == "" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, "invalid webhook body", http.StatusBadRequest)
		return
	}
	typ, id, err := claudemanaged.VerifyWebhook(s.Config.AnthropicWebhookSigningKey, body, r.Header)
	if err != nil {
		http.Error(w, "invalid webhook signature", http.StatusBadRequest)
		return
	}
	if typ == "session.status_idled" || typ == "session.status_terminated" {
		var jobs []store.JobRow
		err = s.Store.Read(r.Context(), func(q *store.Queries) error { var err error; jobs, err = q.JobsBySession(id); return err })
		if err == nil && id != "" {
			for _, job := range jobs {
				if job.Data.Run != nil {
					if e := s.reconcileAgentJob(r.Context(), job); e != nil {
						err = errors.Join(err, s.databaseError(r.Context(), e))
						if s.fatalError.Load() != nil {
							break
						}
					}
				}
			}
		}
		if err != nil {
			err = s.databaseError(r.Context(), err)
			slog.Error("agent webhook reconciliation deferred", "session", id, "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
