package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/store"
)

type failWikiPublication struct{ blob.Store }

func (b failWikiPublication) Put(ctx context.Context, key blob.Key, body []byte) error {
	if strings.Contains(string(key), "/.kioku/wiki/") {
		return errors.New("publication unavailable")
	}
	return b.Store.Put(ctx, key, body)
}
func TestManagedPublicationFailurePreservesFinalUsage(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Final result")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	objects := f.s.Blobs
	f.s.Blobs = failWikiPublication{objects}
	f.s.storage.objects = f.s.Blobs
	deferredPublication(t, f, j)
	if storedJob(t, f, j.ID).Run.CostCents != nil {
		t.Fatal("usage recorded before publication/cleanup")
	}
	f.s.Blobs = objects
	f.s.storage.objects = objects
	restartManagedFixture(t, f)
	if got := reconcileManaged(t, f, j); got.Status != "completed" {
		t.Fatal(got)
	}
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if fixtureJobsGet(t, st, j.ID).Status != "completed" {
			t.Error("publication did not recover")
		}
		if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 123 {
			t.Error("final usage discarded on publication failure")
		}
		return nil
	})
}
