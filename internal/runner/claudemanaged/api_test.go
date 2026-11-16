package claudemanaged_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/runner/claudemanaged"
	"github.com/mash/kioku/internal/runner/claudemanaged/fake"
)

func newClient(t *testing.T) (*claudemanaged.API, *fake.Server) {
	t.Helper()
	f := fake.New()
	t.Cleanup(f.Close)
	return claudemanaged.NewAPI(f.URL(), "test-api-key", f.HTTP.Client()), f
}
func createSession(t *testing.T, a *claudemanaged.API, job string) string {
	t.Helper()
	id, err := a.CreateSession(context.Background(), claudemanaged.SessionInput{MAAgentID: "agent", EnvironmentID: "env", InputFileID: "input", BudgetCents: 1000, Metadata: map[string]string{"kioku_job_id": job}, FirstMessage: "Input and output URLs"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func TestClientPaginatesAndUsesEventTimestamps(t *testing.T) {
	a, f := newClient(t)
	f.SetPageSize(1)
	createSession(t, a, "other")
	old := createSession(t, a, "target")
	latest := createSession(t, a, "target")
	archived := f.Session(old)
	archived.Archived = true
	f.SetSession(archived)
	s := f.Session(latest)
	s.Events = []map[string]any{
		{"id": "e3", "type": "session.status_idle", "processed_at": "2026-11-01T03:00:00Z", "stop_reason": map[string]string{"type": "budget_reached"}},
		{"id": "e1", "type": "session.status_idle", "processed_at": "2026-11-01T01:00:00Z", "stop_reason": map[string]string{"type": "end_turn"}},
		{"id": "e4", "type": "agent.message", "processed_at": "2026-11-01T04:00:00Z"},
		{"id": "e2", "type": "session.status_idle", "processed_at": "2026-11-01T02:00:00Z", "stop_reason": map[string]string{"type": "requires_action"}},
	}
	f.SetSession(s)
	if ids, err := a.FindAttemptSessions(t.Context(), runner.AttemptKey{JobID: "target", ProjectID: "project"}); err != nil || len(ids) != 2 || !slices.Contains(ids, old) || !slices.Contains(ids, latest) {
		t.Fatal(ids, err)
	}
	if ids, err := a.FindAttemptSessions(t.Context(), runner.AttemptKey{JobID: "absent", ProjectID: "project"}); err != nil || len(ids) != 0 {
		t.Fatal(ids, err)
	}
	got, err := a.GetSession(context.Background(), latest)
	if err != nil || got.LastStopReason != "budget_reached" {
		t.Fatal(got, err)
	}
	pages := 0
	for _, r := range f.Requests() {
		if strings.HasSuffix(r.Path, "/events") {
			pages++
			if r.Query.Get("order") != "" {
				t.Fatal("relied on event ordering")
			}
		}
	}
	if pages != 4 {
		t.Fatal(pages)
	}
}
func TestClientFailuresAndInvalidCosts(t *testing.T) {
	a, f := newClient(t)
	id := createSession(t, a, "job")
	for _, code := range []int{429, 503, 400} {
		f.SetError("GET", "/v1/sessions/"+id, code)
		_, err := a.GetSession(context.Background(), id)
		if err == nil || errors.Is(err, claudemanaged.ErrTemporary) != (code != 400) || strings.Contains(err.Error(), "secrets") {
			t.Fatal(err)
		}
	}
	f.SetError("GET", "/v1/sessions/"+id, 0)
	for _, cost := range []string{"", "-1", "1.2", "9223372036854775808"} {
		s := f.Session(id)
		s.CostAmount = cost
		f.SetSession(s)
		if got, err := a.GetSession(context.Background(), id); err != nil || got.Usage.CostCents != nil {
			t.Fatal("invalid cost must remain unknown", got, err)
		}
	}
	s := f.Session(id)
	s.CostAmount = "123"
	s.Currency = "EUR"
	f.SetSession(s)
	if got, err := a.GetSession(context.Background(), id); err != nil || got.Usage.CostCents != nil {
		t.Fatal("non-USD cost must remain unknown", got, err)
	}
	before := len(f.Requests())
	f.SetError("POST", "/v1/sessions", 503)
	if _, err := a.CreateSession(context.Background(), claudemanaged.SessionInput{BudgetCents: 50}); !errors.Is(err, claudemanaged.ErrTemporary) {
		t.Fatal(err)
	}
	if len(f.Requests()) != before+1 {
		t.Fatal("creation was retried")
	}
	f.HTTP.Close()
	if _, err := a.GetSession(context.Background(), id); !errors.Is(err, claudemanaged.ErrTemporary) {
		t.Fatal(err)
	}
}
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer origin.Close()
	a := claudemanaged.NewAPI(origin.URL, "test", origin.Client())
	if _, err := a.CreateSession(context.Background(), claudemanaged.SessionInput{BudgetCents: 50}); err == nil {
		t.Fatal("redirect accepted")
	}
	if received.Load() != 0 {
		t.Fatal("credential followed redirect")
	}
}

func TestClientCleanupTreatsOnlyNotFoundAsSuccess(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden} {
		f := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"missing"}}`))
		}))
		a := claudemanaged.NewAPI(f.URL, "test-key", f.Client())
		for _, operation := range []func(context.Context, string) error{a.DeleteFile, a.DeleteSession, a.Interrupt} {
			err := operation(t.Context(), "missing")
			if (err == nil) != (status == http.StatusNotFound) {
				t.Errorf("HTTP %d: %v", status, err)
			}
		}
		f.Close()
	}
}
