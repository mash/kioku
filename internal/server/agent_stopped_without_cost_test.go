package server

import (
	"context"
	"testing"
	"time"

	"github.com/mash/kioku/internal/store"
)

func TestManagedStoppedWithoutCostCompletesCleanup(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	session := ma.Session(j.Run.Ref.ID)
	session.Status = "terminated"
	session.CostAmount = ""
	ma.SetSession(session)
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		current := fixtureJobsGet(t, st, j.ID)
		current.Run.Deadline = time.Now().Add(-time.Minute)
		fixtureJobsPut(t, st, j.ID, current)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	j = reconcileManaged(t, f, j)
	restartManagedFixture(t, f)
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" || !j.Run.CleanupComplete {
		t.Fatalf("stopped session retained: status=%s cleanup=%v", j.Status, j.Run.CleanupComplete)
	}
	assertAgentCleanup(t, f, ma, j)
	if err := f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 0 {
			t.Error("unknown usage recorded a cost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
