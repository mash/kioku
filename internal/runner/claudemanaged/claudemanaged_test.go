package claudemanaged

import (
	"errors"
	"testing"

	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/runner/claudemanaged/fake"
)

func fixture(t *testing.T) (*Runner, *fake.Server, runner.RunSpec) {
	t.Helper()
	f := fake.New()
	t.Cleanup(f.Close)
	rendered := defaultPrompts(t)
	r := New(NewAPI(f.URL(), "key", nil), "agent", 3, "environment", rendered.System(), rendered.Model())
	return r, f, runner.RunSpec{Key: runner.AttemptKey{JobID: "job", AttemptID: "attempt", ProjectID: "project"}, InputArchive: []byte("input"), SaveRef: func(runner.RunRef) error { return nil }, BudgetCents: 50, FirstMessage: "message"}
}
func start(t *testing.T, r *Runner, spec runner.RunSpec) runner.RunRef {
	t.Helper()
	ref, err := r.Start(t.Context(), spec)
	if err != nil || ref.ID == "" {
		t.Fatal(ref, err)
	}
	return ref
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
		{"idle", "end_turn", runner.StateStopped, ""}, {"idle", "budget_reached", runner.StateStopped, "Claude session did not finish successfully"},
		{"idle", "retries_exhausted", runner.StateStopped, "Claude session did not finish successfully"}, {"idle", "requires_action", runner.StateStopped, "Claude session did not finish successfully"},
		{"idle", "other", runner.StateStopped, "Claude session did not finish successfully"}, {"terminated", "", runner.StateStopped, "Claude session terminated"},
		{"other", "", runner.StateUnknown, ""}, {"", "", runner.StateUnknown, ""},
	} {
		t.Run(tc.status+"/"+tc.stop, func(t *testing.T) {
			f.SetStatus(ref.ID, tc.status, tc.stop, 37)
			got, err := r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: ref})
			if err != nil || got.Run.ID != ref.ID || got.State != tc.state || got.Reason != tc.reason || got.Usage.CostCents == nil || *got.Usage.CostCents != 37 {
				t.Fatal(got, err)
			}
		})
	}
	f.SetStatus(ref.ID, "idle", "retries_exhausted", 37)
	session := f.Session(ref.ID)
	session.CostAmount = ""
	f.SetSession(session)
	got, err := r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: ref})
	if err != nil || got.Usage.CostCents != nil || got.State != runner.StateStopped || got.Reason != "Claude session did not finish successfully" {
		t.Fatal(got, err)
	}
	for _, code := range []int{400, 503} {
		f.SetError("GET", "/v1/sessions/"+ref.ID+"/events", code)
		if _, err := r.Inspect(t.Context(), runner.Attempt{Key: spec.Key, Run: ref}); !errors.Is(err, runner.ErrTemporary) {
			t.Fatal(err)
		}
	}
}
