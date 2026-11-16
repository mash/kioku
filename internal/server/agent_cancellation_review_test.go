package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

type cancelOutputRead struct {
	blob.Store
	entered chan struct{}
}

func (b *cancelOutputRead) Get(ctx context.Context, key blob.Key) ([]byte, error) {
	if strings.Contains(string(key), "/.kioku/jobs/") && strings.Contains(string(key), "/output-") {
		close(b.entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.Store.Get(ctx, key)
}
func TestManagedCancellationPreservesFinalizingForRestart(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Recover after shutdown")
	j = storedJob(t, f, j.ID)
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	objects := f.s.Blobs
	paused := &cancelOutputRead{Store: objects, entered: make(chan struct{})}
	f.s.Blobs = paused
	f.s.storage.objects = f.s.Blobs
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.s.reconcileAgentJob(ctx, fixtureRow(j)) }()
	<-paused.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Error("shutdown not deferred", err)
	}
	if got := storedJob(t, f, j.ID); got.Status != "running" || got.Run.Phase != model.PhaseFinalizing {
		t.Error("shutdown failed durable job", got.Status)
	}
	f.s.Blobs = objects
	f.s.storage.objects = f.s.Blobs
	restartManagedFixture(t, f)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" {
		t.Fatal("restart did not finish output", j.Status, j.Error)
	}
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 123 {
			t.Error("shutdown lost cost")
		}
		return nil
	})
}

func TestManagedFinalizingResumesAfterDeadline(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Finish publication")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	objects := f.s.storage.objects
	f.s.storage.objects = failWikiPublication{objects}
	deferredPublication(t, f, j)
	f.s.storage.objects = objects
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		x := fixtureJobsGet(t, st, j.ID)
		x.Run.Phase = model.PhaseFinalizing
		x.Run.Deadline = time.Now().Add(-time.Minute)
		fixtureJobsPut(t, st, x.ID, x)
		return nil
	})
	restartManagedFixture(t, f)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" {
		t.Fatal("expired execution deadline prevented publication recovery", j.Status, j.Error)
	}
}
