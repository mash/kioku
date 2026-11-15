package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/store"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

type testRunner struct {
	runner.Runner
	start   func(context.Context, runner.RunSpec) (runner.RunRef, error)
	inspect func(context.Context, runner.Attempt) (runner.Status, error)
	stop    func(context.Context, runner.Attempt) error
	cleanup func(context.Context, runner.Attempt) error
}

func (r *testRunner) Start(ctx context.Context, s runner.RunSpec) (runner.RunRef, error) {
	return r.start(ctx, s)
}
func (r *testRunner) Inspect(ctx context.Context, a runner.Attempt) (runner.Status, error) {
	return r.inspect(ctx, a)
}
func (r *testRunner) Stop(ctx context.Context, a runner.Attempt) error    { return r.stop(ctx, a) }
func (r *testRunner) Cleanup(ctx context.Context, a runner.Attempt) error { return r.cleanup(ctx, a) }

func TestConcurrentReconcileSerializesCleanup(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	j := startManaged(t, f, token, p.ID, true)
	fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
		row, err := q.Job(j.ID)
		if err != nil {
			return err
		}
		row.Status = "failed"
		return q.PutJob(row)
	}))
	entered, release := make(chan struct{}), make(chan struct{})
	inspections, cleanups := 0, 0
	f.s.SetRunners(testRunners{Runners: f.s.runners, run: &testRunner{
		inspect: func(context.Context, runner.Attempt) (runner.Status, error) {
			inspections++
			return runner.Status{State: runner.StateStopped}, nil
		},
		cleanup: func(ctx context.Context, _ runner.Attempt) error {
			cleanups++
			if cleanups == 1 {
				close(entered)
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}})
	done := make(chan error, 1)
	go func() { done <- f.s.reconcileAgentJob(t.Context(), fixtureRow(j)) }()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	err := f.s.reconcileAgentJob(ctx, fixtureRow(j))
	cancel()
	close(release)
	fixtureCheck(t, <-done)
	if !errors.Is(err, context.DeadlineExceeded) || inspections != 1 || cleanups != 1 {
		t.Fatalf("concurrent cleanup: inspections=%d cleanups=%d error=%v", inspections, cleanups, err)
	}
	fixtureCheck(t, f.s.reconcileAgentJob(t.Context(), fixtureRow(j)))
	if cleanups != 1 {
		t.Fatal("completed cleanup ran again")
	}
}

func TestReconcileDecisionTableAndSingleInspection(t *testing.T) {
	for _, tc := range []struct {
		name                                        string
		state                                       runner.State
		phase                                       model.ManagedPhase
		output, abort, terminal, old, expired, lost bool
		inspectErr                                  error
		want                                        string
		stops, cleanups                             int
	}{
		{name: "running", state: runner.StateRunning, want: "running"},
		{name: "unknown", state: runner.StateUnknown, want: "running"},
		{name: "located by key", state: runner.StateRunning, lost: true, want: "running"},
		{name: "terminal cleanup", state: runner.StateStopped, terminal: true, want: "failed", cleanups: 1},
		{name: "late live run cleanup", state: runner.StateRunning, terminal: true, want: "failed", stops: 1},
		{name: "abort before output", state: runner.StateStopped, output: true, abort: true, want: "failed", cleanups: 1},
		{name: "temporary inspection", inspectErr: runner.ErrTemporary, want: "running"},
		{name: "permanent inspection", inspectErr: errors.New("inspect failed"), want: "running"},
		{name: "stopped output regardless of reason", state: runner.StateStopped, output: true, want: "completed", cleanups: 1},
		{name: "finalizing", state: runner.StateStopped, output: true, phase: model.PhaseFinalizing, want: "completed", cleanups: 1},
		{name: "stopped without output", state: runner.StateStopped, want: "failed", cleanups: 1},
		{name: "none grace", state: runner.StateNone, want: "running"},
		{name: "none expired", state: runner.StateNone, old: true, want: "failed", cleanups: 1},
		{name: "running deadline", state: runner.StateRunning, expired: true, want: "running", stops: 1},
		{name: "unknown deadline", state: runner.StateUnknown, expired: true, want: "running", stops: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			j := startManaged(t, f, token, p.ID, true)
			if tc.output {
				sendOutput(t, f, ma, j, "Output")
			}
			_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				x := fixtureJobsGet(t, st, j.ID)
				if tc.phase != "" {
					x.Run.Phase = tc.phase
				}
				if tc.abort {
					x.Run.AbortReason = "aborted"
				}
				if tc.terminal {
					x.Status = "failed"
				}
				if tc.old {
					x.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
				}
				if tc.expired {
					x.Run.Deadline = time.Now().Add(-time.Minute)
				}
				if tc.lost {
					x.Run.Ref = runner.RunRef{}
					x.Run.Phase = model.PhasePreparing
				}
				fixtureJobsPut(t, st, x.ID, x)
				return nil
			})
			inspections, stops, cleanups := 0, 0, 0
			cents := int64(71)
			r := &testRunner{
				inspect: func(_ context.Context, a runner.Attempt) (runner.Status, error) {
					inspections++
					return runner.Status{Run: runner.RunRef{ID: j.Run.Ref.ID}, State: tc.state, Reason: "agent failed", Usage: runner.UsageSample{CostCents: &cents}}, tc.inspectErr
				},
				stop:    func(context.Context, runner.Attempt) error { stops++; return nil },
				cleanup: func(context.Context, runner.Attempt) error { cleanups++; return nil },
			}
			f.s.SetRunners(testRunners{Runners: f.s.runners, run: r})
			err := f.s.reconcileAgentJob(t.Context(), fixtureRow(j))
			if err != nil && !errors.Is(err, runner.ErrTemporary) {
				t.Fatal(err)
			}
			got := storedJob(t, f, j.ID)
			status := got.Status
			if got.WikiStatus == "completed" {
				status = got.WikiStatus
			}
			if status != tc.want || inspections != 1 || stops != tc.stops || cleanups != tc.cleanups {
				t.Fatalf("job=%+v calls=%d/%d/%d err=%v", got, inspections, stops, cleanups, err)
			}
			if tc.lost && (got.Run.Ref.ID != j.Run.Ref.ID || got.Run.Phase != model.PhaseAgentRunning) {
				t.Fatal(got.Run)
			}
			if tc.abort && got.Error != "aborted" {
				t.Fatal(got.Error)
			}
			if tc.old && got.Error != "session creation did not finish" {
				t.Fatal(got.Error)
			}
			if tc.expired && (got.Run.AbortReason != "agent job timed out" || got.Run.CostCents != nil) {
				t.Fatal(got.Run)
			}
		})
	}
}
func TestReconcileUnavailableRunner(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	j := startManaged(t, f, token, p.ID, true)
	f.runnerConfig = runners.Config{}
	f.setRunners()
	j = reconcileManaged(t, f, j)
	if j.Status != "running" {
		t.Fatal(j)
	}
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		x := fixtureJobsGet(t, st, j.ID)
		x.Run.Deadline = time.Now().Add(-11 * time.Minute)
		fixtureJobsPut(t, st, j.ID, x)
		return nil
	})
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" || j.Error != "managed agents unavailable; job timed out" || j.Run.CostCents != nil || j.Run.CleanupComplete {
		t.Fatal(j)
	}
}
func TestInspectFailureExpiresAtTokenDeadline(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	j := startManaged(t, f, token, p.ID, true)
	raw := ma.Credential(j.Run.Ref.Resources["vault_id"])
	inspections, stops := 0, 0
	f.s.SetRunners(testRunners{Runners: f.s.runners, run: &testRunner{inspect: func(context.Context, runner.Attempt) (runner.Status, error) {
		inspections++
		c := int64(999)
		return runner.Status{State: runner.StateStopped, Usage: runner.UsageSample{CostCents: &c}}, errors.New("permanent failure")
	}, stop: func(context.Context, runner.Attempt) error { stops++; return nil }, cleanup: func(context.Context, runner.Attempt) error { t.Fatal("unknown run must not be cleaned"); return nil }}})
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		x := fixtureJobsGet(t, st, j.ID)
		x.Run.Deadline = time.Now().Add(-time.Minute)
		fixtureJobsPut(t, st, j.ID, x)
		return nil
	})
	for range 2 {
		if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(j)); !errors.Is(err, runner.ErrTemporary) {
			t.Fatal(err)
		}
	}
	if got := storedJob(t, f, j.ID); got.Status != "running" || got.Run.AbortReason == "" {
		t.Fatal(got)
	}
	if c, _ := agentRequest(t, f, j, raw, "PUT", "output", nil); c != 403 {
		t.Fatal(c)
	}
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		x := fixtureJobsGet(t, st, j.ID)
		x.Run.Deadline = time.Now().Add(-11 * time.Minute)
		fixtureJobsPut(t, st, j.ID, x)
		return nil
	})
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" || j.Run.CostCents != nil || j.Run.CleanupComplete || inspections != 3 || stops != 3 {
		t.Fatal(j, inspections, stops)
	}
}

func TestRunRefSaveFailureRecoversByKey(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	var restore func()
	j := managedJob(t, f, token, p.ID, true)
	real, err := f.s.runners.New(fixtureRow(j))
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetRunners(testRunners{Runners: f.s.runners, run: &testRunner{start: func(ctx context.Context, spec runner.RunSpec) (runner.RunRef, error) {
		ref, err := real.Start(ctx, spec)
		if err == nil {
			restore = fixtureSQLFault(t, f, "jobs", "UPDATE", "NEW.session_id IS NOT NULL")
		}
		return ref, err
	}}})
	if err := f.s.startAgentJob(t.Context(), fixtureRow(j)); err == nil {
		t.Fatal("expected SQL save failure")
	}
	restore()
	restartManagedFixture(t, f)
	j = storedJob(t, f, j.ID)
	if j.Run.Ref.ID != "" || j.Run.Phase != model.PhasePreparing {
		t.Fatal(j.Run)
	}
	j = reconcileManaged(t, f, j)
	if j.Run.Ref.ID == "" || j.Run.Phase != model.PhaseAgentRunning || ma.Session(j.Run.Ref.ID).Interrupted {
		t.Fatal(j.Run)
	}
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 42)
	// Recovery finds the vault during cleanup even though Ref has no resources.
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" || !j.Run.CleanupComplete || !ma.VaultArchived("vault_1") {
		t.Fatal(j)
	}
}

func TestStoppedFailurePreservesCostAfterTokenExpiry(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	j := startManaged(t, f, token, p.ID, true)
	ma.SetStatus(j.Run.Ref.ID, "terminated", "", 137)
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		v := fixtureJobsGet(t, st, j.ID)
		v.Run.Deadline = time.Now().Add(-11 * time.Minute)
		v.Run.AbortReason = "generation deadline exceeded"
		fixtureJobsPut(t, st, v.ID, v)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := reconcileManaged(t, f, j)
	if got.Status != "failed" || got.Run.CostCents == nil || *got.Run.CostCents != 137 {
		t.Fatalf("confirmed cost lost: %+v", got)
	}
}

type testRunners struct {
	Runners
	run runner.Runner
}

func (g testRunners) New(store.JobRow) (runner.Runner, error) { return g.run, nil }
