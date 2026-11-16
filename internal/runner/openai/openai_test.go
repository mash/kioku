package openai

import (
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
	return runner.RunSpec{Key: runner.AttemptKey{JobID: "job", AttemptID: "attempt", ProjectID: "project"}, InputArchive: []byte("input"), SaveRef: func(runner.RunRef) error { return nil }, FirstMessage: "generate wiki", BudgetCents: 50, Deadline: time.Now().Add(time.Hour)}
}
func setup(t *testing.T, h http.HandlerFunc) *Runner {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return New(Config{APIKey: "api-secret", Model: "test-model", Prompt: "wiki instructions", BaseURL: s.URL})
}
func TestInspectDoesNotTreatIdleAsSuccess(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"", "completed", "failed", "cancelled", "in_progress"} {
		t.Run(state, func(t *testing.T) {
			r := setup(t, func(w http.ResponseWriter, q *http.Request) {
				if strings.HasSuffix(q.URL.Path, "/turns") {
					if state == "" {
						fmt.Fprint(w, `{"data":[]}`)
					} else {
						fmt.Fprintf(w, `{"data":[{"id":"turn","status":%q}]}`, state)
					}
				} else {
					fmt.Fprint(w, `{"id":"session","status":"idle","metadata":{"kioku_job_id":"job","kioku_attempt_id":"attempt"},"environment":{"type":"openai_hosted","network":{"access":"disabled"}}}`)
				}
			})
			got, err := r.Inspect(t.Context(), runner.Attempt{Key: spec().Key, Run: runner.RunRef{ID: "session"}})
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
func TestErrorsAreRedactedAndNotRetried(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	hit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { hit = true }))
	defer target.Close()
	r := setup(t, func(w http.ResponseWriter, q *http.Request) { http.Redirect(w, q, target.URL, 307) })
	if _, err := r.Start(t.Context(), spec()); err == nil || hit {
		t.Fatal(err, hit)
	}
}
func TestCleanupWaitsForSessionDeletion(t *testing.T) {
	t.Parallel()
	r := setup(t, func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/agents/sessions/session/artifacts" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		if q.Method == "DELETE" && q.URL.Path == "/agents/sessions/session" {
			w.WriteHeader(409)
			return
		}
		t.Error("must not delete input while session deletion is pending")
		w.WriteHeader(500)
	})
	err := r.Cleanup(t.Context(), runner.Attempt{Key: spec().Key, Run: runner.RunRef{ID: "session", Resources: map[string]string{"input_file_id": "input"}}})
	if !errors.Is(err, runner.ErrTemporary) {
		t.Fatal(err)
	}
}

// Cleanup may delete the session successfully before input deletion fails.
// A later inspection must permit reconciliation to retry input cleanup.
func TestDeletedSessionAllowsCleanupRetry(t *testing.T) {
	t.Parallel()
	r := setup(t, func(w http.ResponseWriter, q *http.Request) { w.WriteHeader(404) })
	got, err := r.Inspect(t.Context(), runner.Attempt{Key: spec().Key, Run: runner.RunRef{ID: "session"}})
	if err != nil || got.State != runner.StateNone {
		t.Fatal(got, err)
	}
}

func TestLostCreateResponseIsRecoveredByAttempt(t *testing.T) {
	t.Parallel()
	created := false
	r := setup(t, func(w http.ResponseWriter, q *http.Request) {
		switch q.Method + " " + q.URL.Path {
		case "POST /files":
			fmt.Fprint(w, `{"id":"input"}`)
		case "POST /agents/sessions":
			created = true
			fmt.Fprint(w, `{"id":`)
		case "GET /agents/sessions":
			if !created {
				t.Error("session not created")
			}
			fmt.Fprint(w, `{"data":[{"id":"session","metadata":{"kioku_job_id":"job","kioku_attempt_id":"attempt"}}]}`)
		case "GET /agents/sessions/session":
			fmt.Fprint(w, `{"id":"session","status":"in_progress","metadata":{"kioku_job_id":"job","kioku_attempt_id":"attempt"},"environment":{"type":"openai_hosted","network":{"access":"disabled"}}}`)
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
	if err != nil || got.State != runner.StateRunning || got.Run.ID != "session" {
		t.Fatal(got, err)
	}
}
