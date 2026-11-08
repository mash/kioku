package server

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner/claudemanaged"
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
		var job model.Job
		err = s.DB.View(func(st model.State) error {
			for _, j := range st.Jobs {
				if j.Status == "running" && j.Run != nil && j.Run.Ref.ID == id && id != "" {
					job = j
					break
				}
			}
			return nil
		})
		if err == nil && job.ID != "" {
			err = s.reconcileAgentJob(r.Context(), job)
		}
		if err != nil {
			slog.Error("agent webhook reconciliation deferred", "job", job.ID, "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
