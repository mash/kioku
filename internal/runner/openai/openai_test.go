package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/runner"
)

func spec() runner.RunSpec {
	return runner.RunSpec{Key: runner.AttemptKey{JobID: "job", AttemptID: "attempt", ProjectID: "project"}, InputURL: "https://kioku.test/input", OutputURL: "https://kioku.test/output", Token: "job-secret", FirstMessage: "generate wiki", BudgetCents: 50, Deadline: time.Now().Add(time.Hour)}
}
func setup(t *testing.T, h http.HandlerFunc) *Runner {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return New(Config{APIKey: "api-secret", Model: "test-model", Prompt: "wiki instructions", BaseURL: s.URL})
}
func TestLifecycle(t *testing.T) {
	var calls []string
	r := setup(t, func(w http.ResponseWriter, q *http.Request) {
		calls = append(calls, q.Method+" "+q.URL.Path)
		if q.Header.Get("Authorization") != "Bearer api-secret" || q.Header.Get("OpenAI-Beta") != "agents=v1" {
			t.Error("missing authentication")
		}
		switch q.Method + " " + q.URL.Path {
		case "POST /vaults":
			fmt.Fprint(w, `{"id":"vault"}`)
		case "POST /vaults/vault/credentials":
			// Inspect wire names, not Go field inference.
			var raw map[string]any
			json.NewDecoder(q.Body).Decode(&raw)
			auth := raw["auth"].(map[string]any)
			if auth["secret_name"] != "KIOKU_JOB_TOKEN" || auth["secret_value"] != "job-secret" || auth["type"] != "environment_variable" {
				t.Error(raw)
			}
			fmt.Fprint(w, `{"id":"credential"}`)
		case "POST /agents/sessions":
			var b map[string]any
			json.NewDecoder(q.Body).Decode(&b)
			if b["spend_control"].(map[string]any)["limit"] != float64(50) {
				t.Error(b)
			}
			env := b["environment"].(map[string]any)
			if env["network"].(map[string]any)["access"] != "restricted" {
				t.Error(b)
			}
			encoded, _ := json.Marshal(b)
			if strings.Contains(string(encoded), "job-secret") || strings.Contains(string(encoded), "api-secret") {
				t.Error("secret in session")
			}
			fmt.Fprint(w, `{"id":"session"}`)
		case "GET /agents/sessions/session":
			fmt.Fprint(w, `{"id":"session","status":"idle","vault_ids":["vault"],"spend_control":{"consumed":42,"limit":50}}`)
		case "GET /agents/sessions/session/turns":
			fmt.Fprint(w, `{"data":[{"status":"completed"}]}`)
		case "POST /agents/sessions/session/events":
			var b map[string]any
			json.NewDecoder(q.Body).Decode(&b)
			if b["events"].([]any)[0].(map[string]any)["type"] != "agent.session.input.cancel" {
				t.Error(b)
			}
			w.WriteHeader(204)
		case "GET /agents/sessions", "GET /vaults":
			fmt.Fprint(w, `{"data":[],"has_more":false}`)
		case "DELETE /agents/sessions/session", "DELETE /vaults/vault":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", q.Method, q.URL)
			w.WriteHeader(500)
		}
	})
	ref, err := r.Start(t.Context(), spec())
	if err != nil {
		t.Fatal(err)
	}
	a := runner.Attempt{Key: spec().Key, Run: ref}
	status, err := r.Inspect(t.Context(), a)
	if err != nil || status.State != runner.StateStopped || status.Cost != nil {
		t.Fatal(status, err)
	}
	if err = r.Stop(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if err = r.Cleanup(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "DELETE /vaults/vault" {
		t.Fatal(calls)
	}
}
func TestInspectDoesNotTreatIdleAsSuccess(t *testing.T) {
	for _, state := range []string{"", "completed", "failed", "cancelled", "in_progress"} {
		t.Run(state, func(t *testing.T) {
			r := setup(t, func(w http.ResponseWriter, q *http.Request) {
				if strings.HasSuffix(q.URL.Path, "/turns") {
					if state == "" {
						fmt.Fprint(w, `{"data":[]}`)
					} else {
						fmt.Fprintf(w, `{"data":[{"status":%q}]}`, state)
					}
				} else {
					fmt.Fprint(w, `{"id":"session","status":"idle"}`)
				}
			})
			got, err := r.Inspect(t.Context(), runner.Attempt{Run: runner.RunRef{ID: "session"}})
			if err != nil {
				t.Fatal(err)
			}
			stopped := state == "completed" || state == "failed" || state == "cancelled"
			if (got.State == runner.StateStopped) != stopped {
				t.Fatal(got)
			}
			if (state == "failed" || state == "cancelled") && got.Reason == "" {
				t.Fatal(got)
			}
		})
	}
}
func TestDiscoveryAndCleanupPartialStarts(t *testing.T) {
	var deleted []string
	r := setup(t, func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "DELETE" {
			deleted = append(deleted, q.URL.Path)
			w.WriteHeader(404)
			return
		}
		if q.URL.Path == "/agents/sessions" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		if q.URL.Query().Get("after") == "" {
			fmt.Fprint(w, `{"data":[{"id":"foreign","metadata":{"kioku_job_id":"job","kioku_attempt_id":"other","kioku_project_id":"project"}}],"has_more":true,"last_id":"foreign"}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"orphan","metadata":{"kioku_job_id":"job","kioku_attempt_id":"attempt","kioku_project_id":"project"}}]}`)
	})
	if err := r.Cleanup(t.Context(), runner.Attempt{Key: spec().Key}); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "/vaults/orphan" {
		t.Fatal(deleted)
	}
}
func TestErrorsAreRedactedAndNotRetried(t *testing.T) {
	for _, code := range []int{400, 401, 409, 429, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			calls := 0
			r := setup(t, func(w http.ResponseWriter, q *http.Request) {
				calls++
				w.WriteHeader(code)
				fmt.Fprint(w, "api-secret job-secret")
			})
			_, err := r.Start(t.Context(), spec())
			if err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
				t.Fatal(err, calls)
			}
			if errors.Is(err, runner.ErrTemporary) != (code == 409 || code == 429 || code >= 500) {
				t.Fatal(err)
			}
		})
	}
}
func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	hit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { hit = true }))
	defer target.Close()
	r := setup(t, func(w http.ResponseWriter, q *http.Request) { http.Redirect(w, q, target.URL, 307) })
	if _, err := r.Start(t.Context(), spec()); err == nil || hit {
		t.Fatal(err, hit)
	}
}
func TestInvalidJobDoesNotCallProvider(t *testing.T) {
	r := setup(t, func(w http.ResponseWriter, q *http.Request) { t.Error("unexpected provider call") })
	for _, u := range []string{"http://kioku.test/input", "https://user:pass@kioku.test/input", "https://kioku.test:8080/input", "https://other.test/input"} {
		s := spec()
		s.InputURL = u
		if _, err := r.Start(t.Context(), s); err == nil {
			t.Fatal(u)
		}
	}
}
func TestCleanupWaitsForSessionDeletion(t *testing.T) {
	r := setup(t, func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/agents/sessions" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		if q.Method == "DELETE" && q.URL.Path == "/agents/sessions/session" {
			w.WriteHeader(409)
			return
		}
		t.Error("must not delete vault while session deletion is pending")
		w.WriteHeader(500)
	})
	err := r.Cleanup(t.Context(), runner.Attempt{Key: spec().Key, Run: runner.RunRef{ID: "session", Resources: map[string]string{"vault_id": "vault"}}})
	if !errors.Is(err, runner.ErrTemporary) {
		t.Fatal(err)
	}
}

// Cleanup may delete the session successfully before vault deletion fails.
// A later inspection must permit reconciliation to retry vault cleanup.
func TestDeletedSessionAllowsCleanupRetry(t *testing.T) {
	r := setup(t, func(w http.ResponseWriter, q *http.Request) { w.WriteHeader(404) })
	got, err := r.Inspect(t.Context(), runner.Attempt{Run: runner.RunRef{ID: "session"}})
	if err != nil || got.State != runner.StateNone {
		t.Fatal(got, err)
	}
}

func TestLostCreateResponseIsRecoveredByAttempt(t *testing.T) {
	created := false
	r := setup(t, func(w http.ResponseWriter, q *http.Request) {
		switch q.Method + " " + q.URL.Path {
		case "POST /vaults":
			fmt.Fprint(w, `{"id":"vault"}`)
		case "POST /vaults/vault/credentials":
			w.WriteHeader(204)
		case "POST /agents/sessions":
			created = true
			fmt.Fprint(w, `{"id":`)
		case "GET /agents/sessions":
			if !created {
				t.Error("session not created")
			}
			fmt.Fprint(w, `{"data":[{"id":"session","metadata":{"kioku_job_id":"job","kioku_attempt_id":"attempt","kioku_project_id":"project"}}]}`)
		case "GET /agents/sessions/session":
			fmt.Fprint(w, `{"id":"session","status":"in_progress","vault_ids":["vault"]}`)
		default:
			t.Errorf("unexpected request: %s", q.URL)
			w.WriteHeader(500)
		}
	})
	_, err := r.Start(t.Context(), spec())
	if !errors.Is(err, runner.ErrTemporary) {
		t.Fatal(err)
	}
	got, err := r.Inspect(t.Context(), runner.Attempt{Key: spec().Key})
	if err != nil || got.State != runner.StateRunning || got.Run.ID != "session" || got.Run.Resources["vault_id"] != "vault" {
		t.Fatal(got, err)
	}
}
