package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/store"

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
	t.Parallel()
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
	f.s.SetRunner(&testRunner{
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
	})
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

func TestReconcileUnavailableRunner(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	f, token, p, _ := managedFixture(t)
	j := startManaged(t, f, token, p.ID, true)
	inspections, stops := 0, 0
	f.s.SetRunner(&testRunner{inspect: func(context.Context, runner.Attempt) (runner.Status, error) {
		inspections++
		c := int64(999)
		return runner.Status{State: runner.StateStopped, Usage: runner.UsageSample{CostCents: &c}}, errors.New("permanent failure")
	}, stop: func(context.Context, runner.Attempt) error { stops++; return nil }, cleanup: func(context.Context, runner.Attempt) error { t.Fatal("unknown run must not be cleaned"); return nil }})
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

func TestStoppedFailurePreservesCostAfterTokenExpiry(t *testing.T) {
	t.Parallel()
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
