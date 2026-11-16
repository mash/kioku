package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mash/kioku/internal/runner/claudemanaged"
	"github.com/mash/kioku/internal/store"
)

func TestManagedInterruptFailureDefersFinalCost(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(j.Run.Ref.ID, "running", "", 100)
	ma.SetError("POST", "/v1/sessions/"+j.Run.Ref.ID+"/events", 503)
	ma.SetError("DELETE", "/v1/sessions/"+j.Run.Ref.ID, 503)
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		x := fixtureJobsGet(t, st, j.ID)
		x.Run.Deadline = time.Now().Add(-time.Second)
		fixtureJobsPut(t, st, x.ID, x)
		return nil
	})
	err := f.s.reconcileAgentJob(context.Background(), fixtureRow(j))
	if err != nil && !errors.Is(err, claudemanaged.ErrTemporary) {
		t.Fatal(err)
	}
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if fixtureJobsGet(t, st, j.ID).Status != "running" {
			t.Error("stopped tracking running session")
		}
		if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 0 {
			t.Error("partial cost recorded")
		}
		return nil
	})
	restartManagedFixture(t, f)
	ma.SetError("POST", "/v1/sessions/"+j.Run.Ref.ID+"/events", 0)
	ma.SetError("DELETE", "/v1/sessions/"+j.Run.Ref.ID, 0)
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 200)
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" {
		t.Fatal(j)
	}
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 200 {
			t.Error("final usage lost")
		}
		return nil
	})
	assertAgentCleanup(t, f, ma, j)
}
