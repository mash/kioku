package claudemanaged

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mash/kioku/internal/runner"
)

type partialCleanupClient struct {
	FileClient
	remaining map[string]bool
	failed    bool
}

func (c *partialCleanupClient) FindAttemptSessions(context.Context, runner.AttemptKey) ([]string, error) {
	var ids []string
	for id := range c.remaining {
		ids = append(ids, id)
	}
	return ids, nil
}
func (c *partialCleanupClient) GetSession(_ context.Context, id string) (Session, error) {
	if !c.remaining[id] {
		return Session{}, &APIError{Status: 404}
	}
	return Session{ID: id, Status: "idle", LastStopReason: "end_turn", Metadata: map[string]string{"kioku_job_id": "job", "kioku_attempt_id": "attempt"}}, nil
}
func (c *partialCleanupClient) DeleteSession(_ context.Context, id string) error {
	if id == "second" && !c.failed {
		c.failed = true
		return &APIError{Status: 503}
	}
	delete(c.remaining, id)
	return nil
}

func TestClaudeCleanupRecoversAfterPartialSessionDeletion(t *testing.T) {
	c := &partialCleanupClient{remaining: map[string]bool{"first": true, "second": true}}
	r := (&Runner{}).WithFiles(c, "")
	a := runner.Attempt{Key: runner.AttemptKey{JobID: "job", AttemptID: "attempt", ProjectID: "project"}, Run: runner.RunRef{ID: "first", UsesProviderFiles: true}}
	status, err := r.Inspect(t.Context(), a)
	if err != nil || status.State != runner.StateStopped {
		t.Fatal(status, err)
	}
	if err := r.Cleanup(t.Context(), a); err == nil || c.remaining["first"] || !c.remaining["second"] {
		t.Fatal("partial deletion was not exercised", err, c.remaining)
	}
	status, err = r.Inspect(t.Context(), a)
	if err != nil || status.State != runner.StateStopped {
		t.Fatalf("deleted session blocks retry: %+v %v", status, err)
	}
	if err := r.Cleanup(t.Context(), a); err != nil || len(c.remaining) != 0 {
		t.Fatal("cleanup did not resume", err, c.remaining)
	}
}

func TestClaudeFileExecutionFailureSurvivesOutputReceipt(t *testing.T) {
	r, f, spec := fixture(t)
	ref := start(t, r, spec)
	for _, state := range []string{"idle", "terminated"} {
		f.SetStatus(ref.ID, state, "retries_exhausted", 40)
		status, err := inspectSessions(t.Context(), NewAPI(f.URL(), "key", nil), runner.Attempt{Key: spec.Key, Run: ref}, []string{ref.ID}, true)
		if err != nil || status.State != runner.StateStopped || !status.Failed {
			t.Fatalf("execution failure would permit saved output publication: %+v %v", status, err)
		}
	}
}

func TestClaudeDiscoveryRejectsEmptySessionID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"","metadata":{"kioku_job_id":"job","kioku_attempt_id":"attempt"}}]}`)
	}))
	defer srv.Close()
	a := NewAPI(srv.URL, "key", srv.Client())
	if ids, err := a.FindAttemptSessions(t.Context(), runner.AttemptKey{JobID: "job", AttemptID: "attempt", ProjectID: "project"}); err == nil {
		t.Fatal("empty session ID silently omitted", ids)
	}
}
