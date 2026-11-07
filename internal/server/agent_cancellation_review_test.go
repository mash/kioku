package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

type cancelOutputRead struct {
	blob.Store
	key     blob.Key
	entered chan struct{}
}

func (b *cancelOutputRead) Get(ctx context.Context, key blob.Key) ([]byte, error) {
	if key == b.key {
		close(b.entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.Store.Get(ctx, key)
}
func TestManagedCancellationPreservesFinalizingForRestart(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Recover after shutdown")
	j = storedJob(t, f, j.ID)
	ma.SetStatus(j.Managed.SessionID, "idle", "end_turn", 123)
	objects := f.s.Blobs
	paused := &cancelOutputRead{Store: objects, key: j.Managed.OutputKey, entered: make(chan struct{})}
	f.s.Blobs = paused
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.s.reconcileAgentJob(ctx, j) }()
	<-paused.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Error("shutdown not deferred", err)
	}
	if got := storedJob(t, f, j.ID); got.Status != "running" || got.Managed.Phase != model.PhaseFinalizing {
		t.Error("shutdown failed durable job", got.Status)
	}
	f.s.Blobs = objects
	restartManagedFixture(t, f)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" {
		t.Fatal("restart did not finish output", j.Status, j.Error)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Jobs[j.ID].Managed.ListCostCents != 123 {
			t.Error("shutdown lost cost")
		}
		return nil
	})
}

func TestManagedFinalizingResumesAfterDeadline(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Finish publication")
	ma.SetStatus(j.Managed.SessionID, "idle", "end_turn", 123)
	_ = f.db.Update(func(st *model.State) error {
		x := st.Jobs[j.ID]
		x.Managed.Phase = model.PhaseFinalizing
		x.Managed.Deadline = time.Now().Add(-time.Minute)
		st.Jobs[x.ID] = x
		p := st.Projects[p.ID]
		p.WikiIncomplete = true
		st.Projects[p.ID] = p
		return nil
	})
	restartManagedFixture(t, f)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" {
		t.Fatal("expired execution deadline prevented publication recovery", j.Status, j.Error)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Projects[p.ID].WikiIncomplete {
			t.Error("publication left incomplete")
		}
		return nil
	})
}
