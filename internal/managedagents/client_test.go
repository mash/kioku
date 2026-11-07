package managedagents_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/managedagents/fake"
)

func newClient(t *testing.T) (*managedagents.API, *fake.Server) {
	t.Helper()
	f := fake.New()
	t.Cleanup(f.Close)
	return managedagents.New(f.URL(), "test-api-key", f.HTTP.Client()), f
}
func createSession(t *testing.T, a *managedagents.API, job string) string {
	t.Helper()
	id, err := a.CreateSession(context.Background(), managedagents.SessionInput{MAAgentID: "agent", EnvironmentID: "env", VaultID: "vault", BudgetCents: 1000, Metadata: map[string]string{"kioku_job_id": job}, FirstMessage: "Input and output URLs"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func TestClientLifecycleAndWireContract(t *testing.T) {
	a, f := newClient(t)
	ctx := context.Background()
	vault, err := a.CreateJobVault(ctx, managedagents.VaultInput{JobID: "job", SecretName: "KIOKU_JOB_TOKEN", SecretValue: "test-job-secret", AllowedHost: "kioku.test"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.CreateSession(ctx, managedagents.SessionInput{MAAgentID: "agent", MAAgentVersion: 3, EnvironmentID: "env", VaultID: vault, BudgetCents: 1234, Title: "Wiki job", Metadata: map[string]string{"kioku_job_id": "job", "kioku_project_id": "project"}, FirstMessage: "Read input then upload output"})
	if err != nil {
		t.Fatal(err)
	}
	f.SetStatus(id, "idle", "end_turn", 123)
	s, err := a.GetSession(ctx, id)
	if err != nil || s.LastStopReason != "end_turn" || s.ListCostCents != 123 || s.Status != "idle" {
		t.Fatal(s, err)
	}
	if got, err := a.FindSessionByJob(ctx, "job"); err != nil || got != id {
		t.Fatal(got, err)
	}
	if err := a.Interrupt(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := a.ArchiveSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := a.ArchiveVault(ctx, vault); err != nil {
		t.Fatal(err)
	}
	if !f.Session(id).Interrupted || !f.Session(id).Archived || !f.VaultArchived(vault) {
		t.Fatal("cleanup not sent")
	}
	foundCredential, foundSession, foundInterrupt := false, false, false
	for _, r := range f.Requests() {
		if r.Header.Get("Authorization") != "Bearer test-api-key" || !strings.Contains(r.Header.Get("anthropic-beta"), "managed-agents-2026-04-01") {
			t.Fatal("missing authentication/beta headers", r.Header)
		}
		switch {
		case strings.HasSuffix(r.Path, "/credentials"):
			foundCredential = true
			auth := r.Body["auth"].(map[string]any)
			network := auth["networking"].(map[string]any)
			injection := auth["injection_location"].(map[string]any)
			if auth["type"] != "environment_variable" || auth["secret_name"] != "KIOKU_JOB_TOKEN" || auth["secret_value"] != "test-job-secret" || network["type"] != "limited" || !reflect.DeepEqual(network["allowed_hosts"], []any{"kioku.test"}) || !reflect.DeepEqual(injection, map[string]any{"header": true}) {
				t.Fatal(r.Body)
			}
		case r.Path == "/v1/sessions" && r.Method == "POST":
			foundSession = true
			budget := r.Body["budget"].(map[string]any)
			money := budget["max_list_cost"].(map[string]any)
			agent := r.Body["agent"].(map[string]any)
			if budget["type"] != "limit" || money["amount"] != "1234" || money["currency"] != "USD" || agent["id"] != "agent" || agent["version"] != float64(3) || r.Body["environment_id"] != "env" || !reflect.DeepEqual(r.Body["vault_ids"], []any{vault}) {
				t.Fatal(r.Body)
			}
			events := r.Body["initial_events"].([]any)
			event := events[0].(map[string]any)
			content := event["content"].([]any)[0].(map[string]any)
			metadata := r.Body["metadata"].(map[string]any)
			if event["type"] != "user.message" || content["text"] != "Read input then upload output" || metadata["kioku_job_id"] != "job" || metadata["kioku_project_id"] != "project" {
				t.Fatal(r.Body)
			}
		case r.Method == "POST" && strings.HasSuffix(r.Path, "/events"):
			foundInterrupt = true
			event := r.Body["events"].([]any)[0].(map[string]any)
			if event["type"] != "user.interrupt" {
				t.Fatal(r.Body)
			}
		}
	}
	if !foundCredential || !foundSession || !foundInterrupt {
		t.Fatal("missing requests")
	}
}
func TestClientPaginatesAndUsesEventTimestamps(t *testing.T) {
	a, f := newClient(t)
	f.SetPageSize(1)
	createSession(t, a, "other")
	old := createSession(t, a, "target")
	latest := createSession(t, a, "target")
	s := f.Session(latest)
	s.Events = []map[string]any{
		{"id": "e3", "type": "session.status_idle", "processed_at": "2026-11-01T03:00:00Z", "stop_reason": map[string]string{"type": "budget_reached"}},
		{"id": "e1", "type": "session.status_idle", "processed_at": "2026-11-01T01:00:00Z", "stop_reason": map[string]string{"type": "end_turn"}},
		{"id": "e4", "type": "agent.message", "processed_at": "2026-11-01T04:00:00Z"},
		{"id": "e2", "type": "session.status_idle", "processed_at": "2026-11-01T02:00:00Z", "stop_reason": map[string]string{"type": "requires_action"}},
	}
	f.SetSession(s)
	if id, err := a.FindSessionByJob(context.Background(), "target"); err != nil || id != latest || id == old {
		t.Fatal(id, err)
	}
	if id, err := a.FindSessionByJob(context.Background(), "absent"); err != nil || id != "" {
		t.Fatal(id, err)
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
		if err == nil || errors.Is(err, managedagents.ErrTemporary) != (code != 400) || strings.Contains(err.Error(), "secrets") {
			t.Fatal(err)
		}
	}
	f.SetError("GET", "/v1/sessions/"+id, 0)
	for _, cost := range []string{"", "-1", "1.2", "9223372036854775807"} {
		s := f.Session(id)
		s.CostAmount = cost
		f.SetSession(s)
		if got, err := a.GetSession(context.Background(), id); err != nil || got.CostKnown {
			t.Fatal("invalid cost must remain unknown", got, err)
		}
	}
	s := f.Session(id)
	s.CostAmount = "123"
	s.Currency = "EUR"
	f.SetSession(s)
	if got, err := a.GetSession(context.Background(), id); err != nil || got.CostKnown {
		t.Fatal("non-USD cost must remain unknown", got, err)
	}
	f.SetError("POST", "/v1/vaults/vault_1/credentials", 503)
	vault, err := a.CreateJobVault(context.Background(), managedagents.VaultInput{JobID: "job", AllowedHost: "kioku.test"})
	if vault != "vault_1" || !errors.Is(err, managedagents.ErrTemporary) {
		t.Fatal("lost vault cleanup handle", vault, err)
	}
	before := len(f.Requests())
	f.SetError("POST", "/v1/sessions", 503)
	if _, err := a.CreateSession(context.Background(), managedagents.SessionInput{BudgetCents: 50}); !errors.Is(err, managedagents.ErrTemporary) {
		t.Fatal(err)
	}
	if len(f.Requests()) != before+1 {
		t.Fatal("creation was retried")
	}
	f.HTTP.Close()
	if _, err := a.GetSession(context.Background(), id); !errors.Is(err, managedagents.ErrTemporary) {
		t.Fatal(err)
	}
}
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer origin.Close()
	a := managedagents.New(origin.URL, "test", origin.Client())
	if _, err := a.CreateSession(context.Background(), managedagents.SessionInput{BudgetCents: 50}); err == nil {
		t.Fatal("redirect accepted")
	}
	if received.Load() != 0 {
		t.Fatal("credential followed redirect")
	}
}
