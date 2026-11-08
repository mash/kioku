package claudemanaged

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/managedagents/fake"
	"github.com/mash/kioku/internal/runner"
)

func fixture(t *testing.T) (*Runner, *fake.Server, runner.RunSpec) {
	t.Helper()
	f := fake.New()
	t.Cleanup(f.Close)
	rendered := defaultPrompts(t)
	r := New(func() (managedagents.Client, error) { return managedagents.New(f.URL(), "key", nil), nil }, "agent", 3, "environment", rendered.System(), rendered.Model())
	return r, f, runner.RunSpec{Key: runner.AttemptKey{JobID: "job", AttemptID: "attempt", ProjectID: "project"}, Token: runner.Secret("test-job-token"), OutputURL: "https://kioku.test:8443/output", BudgetCents: 50, FirstMessage: "message"}
}
func start(t *testing.T, r *Runner, spec runner.RunSpec) runner.RunRef {
	t.Helper()
	ref, err := r.Start(t.Context(), spec)
	if err != nil || ref.ID == "" {
		t.Fatal(ref, err)
	}
	return ref
}
func TestLifecycle(t *testing.T) {
	r, f, spec := fixture(t)
	ref := start(t, r, spec)
	if f.Credential(ref.Resources["vault_id"]) != "test-job-token" {
		t.Fatal("credential lost")
	}
	metadata := map[string]any{"kioku_job_id": "job", "kioku_attempt_id": "attempt"}
	for _, req := range f.Requests() {
		if req.Path == "/v1/sessions" && req.Method == "POST" {
			if req.Body["title"] != "Kioku wiki job" || !strings.Contains(fmt.Sprint(req.Body["initial_events"]), spec.FirstMessage) || !reflect.DeepEqual(req.Body["metadata"], metadata) {
				t.Fatal(req.Body)
			}
		}
		if req.Path == "/v1/vaults" && req.Method == "POST" && !reflect.DeepEqual(req.Body["metadata"], metadata) {
			t.Fatal(req.Body)
		}
		if strings.HasSuffix(req.Path, "/credentials") {
			auth := req.Body["auth"].(map[string]any)
			network := auth["networking"].(map[string]any)
			if auth["secret_name"] != tokenName || !reflect.DeepEqual(network["allowed_hosts"], []any{"kioku.test"}) {
				t.Fatal(auth)
			}
		}
	}
	a := runner.Attempt{Key: spec.Key}
	found, err := r.Inspect(t.Context(), a)
	if err != nil || found.Run.ID != ref.ID || found.State != runner.StateRunning {
		t.Fatal(found, err)
	}
	wrong := a
	wrong.Key.AttemptID = "other"
	found, err = r.Inspect(t.Context(), wrong)
	if err != nil || found.State != runner.StateNone {
		t.Fatal(found, err)
	}
	before := len(f.Requests())
	if err := r.Stop(t.Context(), a); err != nil || len(f.Requests()) != before {
		t.Fatal("empty Stop called API", err)
	}
	a.Run = ref
	if err := r.Stop(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	// Both IDs may be lost before their local commit.
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key}); err != nil {
		t.Fatal(err)
	}
	if !f.Session(ref.ID).Archived || !f.VaultArchived(ref.Resources["vault_id"]) {
		t.Fatal("unrecorded resources not archived")
	}
	if err := r.Cleanup(t.Context(), a); err != nil {
		t.Fatal(err)
	}
}
func TestInspectStates(t *testing.T) {
	r, f, spec := fixture(t)
	ref := start(t, r, spec)
	for _, tc := range []struct {
		status, stop string
		state        runner.State
		reason       string
	}{
		{"running", "", runner.StateRunning, ""}, {"rescheduling", "", runner.StateRunning, ""},
		{"idle", "end_turn", runner.StateStopped, ""}, {"idle", "budget_reached", runner.StateStopped, ""},
		{"idle", "retries_exhausted", runner.StateStopped, "agent gave up after errors"}, {"idle", "requires_action", runner.StateStopped, "agent requires action"},
		{"idle", "other", runner.StateStopped, ""}, {"terminated", "", runner.StateStopped, "session terminated"},
		{"other", "", runner.StateUnknown, ""}, {"", "", runner.StateUnknown, ""},
	} {
		t.Run(tc.status+"/"+tc.stop, func(t *testing.T) {
			f.SetStatus(ref.ID, tc.status, tc.stop, 37)
			got, err := r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: ref})
			if err != nil || got.Run.ID != ref.ID || got.State != tc.state || got.Reason != tc.reason || got.Cost == nil || *got.Cost != 37 {
				t.Fatal(got, err)
			}
		})
	}
	f.SetStatus(ref.ID, "idle", "retries_exhausted", 37)
	session := f.Session(ref.ID)
	session.CostAmount = ""
	f.SetSession(session)
	got, err := r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: ref})
	if err != nil || got.Cost != nil || got.State != runner.StateStopped || got.Reason != "agent gave up after errors" {
		t.Fatal(got, err)
	}
	for _, code := range []int{400, 503} {
		f.SetError("GET", "/v1/sessions/"+ref.ID+"/events", code)
		if _, err := r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); !errors.Is(err, runner.ErrTemporary) {
			t.Fatal(err)
		}
	}
}
func TestStartFailures(t *testing.T) {
	for _, path := range []string{"/v1/vaults", "/v1/vaults/vault_1/credentials", "/v1/sessions"} {
		for _, code := range []int{400, 503} {
			t.Run(fmt.Sprint(path, code), func(t *testing.T) {
				r, f, spec := fixture(t)
				f.SetError("POST", path, code)
				ref, err := r.Start(t.Context(), spec)
				if ref.ID != "" || len(ref.Resources) != 0 || err == nil || errors.Is(err, runner.ErrTemporary) != (code == 503) {
					t.Fatal(ref, err)
				}
			})
		}
	}
}
func TestClientResolvedOnEveryOperation(t *testing.T) {
	r, _, spec := fixture(t)
	ref := start(t, r, spec)
	old := r.client
	calls := 0
	unavailable := false
	sentinel := errors.New("Anthropic connection unavailable; reconnect the original workspace")
	r.client = func() (managedagents.Client, error) {
		calls++
		if unavailable {
			return nil, sentinel
		}
		return old()
	}
	a := runner.Attempt{Key: spec.Key, Run: ref}
	operations := []func() error{
		func() error { _, err := r.Inspect(context.Background(), a); return err },
		func() error { return r.Stop(context.Background(), a) },
		func() error { return r.Cleanup(context.Background(), a) },
		func() error { _, err := r.Start(context.Background(), spec); return err },
	}
	for _, op := range operations {
		before := calls
		if err := op(); err != nil || calls != before+1 {
			t.Fatal(calls, err)
		}
	}
	unavailable = true
	for _, op := range operations {
		before := calls
		if err := op(); !errors.Is(err, sentinel) || calls != before+1 {
			t.Fatal(calls, err)
		}
	}
}
func TestCleanupJoinsFailures(t *testing.T) {
	r, f, spec := fixture(t)
	ref := start(t, r, spec)
	f.SetError("POST", "/v1/sessions/"+ref.ID+"/archive", 503)
	f.SetError("POST", "/v1/vaults/"+ref.Resources["vault_id"]+"/archive", 503)
	err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key, Run: ref})
	if !errors.Is(err, runner.ErrTemporary) || !strings.Contains(err.Error(), "\n") {
		t.Fatal(err)
	}
}

func TestVaultContainsOnlyJobTokenWithHeaderDestination(t *testing.T) {
	r, f, spec := fixture(t)
	ref := start(t, r, spec)
	count := 0
	for _, req := range f.Requests() {
		if strings.HasSuffix(req.Path, "/credentials") {
			if req.Path != "/v1/vaults/"+ref.Resources["vault_id"]+"/credentials" {
				t.Fatal("multiple vaults")
			}
			a := req.Body["auth"].(map[string]any)
			name := a["secret_name"].(string)
			hosts := []any{"kioku.test"}
			if name != "KIOKU_JOB_TOKEN" || a["type"] != "environment_variable" || !reflect.DeepEqual(a["networking"].(map[string]any)["allowed_hosts"], hosts) || !reflect.DeepEqual(a["injection_location"], map[string]any{"header": true}) {
				t.Fatal(a)
			}
			count++
		}
		if req.Path == "/v1/sessions" && strings.Contains(fmt.Sprint(req.Body), string(spec.Token)) {
			t.Fatal("credential in first message")
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); err != nil {
		t.Fatal(err)
	}
	if !f.VaultArchived(ref.Resources["vault_id"]) {
		t.Fatal("vault not archived")
	}
}
