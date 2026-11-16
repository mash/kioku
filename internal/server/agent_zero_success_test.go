package server

import (
	"context"
	"testing"

	"github.com/mash/kioku/internal/store"
)

func TestManagedSuccessfulZeroCostCompletes(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Free completed result")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 0)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" {
		t.Fatal(j)
	}
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 0 {
			t.Error("zero usage changed")
		}
		return nil
	})
	next := managedJob(t, f, token, p.ID, true)
	if err := f.s.startAgentJob(context.Background(), fixtureRow(next)); err != nil {
		t.Fatal(err)
	}
	if got := storedJob(t, f, next.ID); got.Status != "running" || got.Run == nil {
		t.Error("zero success blocked the next run", got.Status, got.Error)
	}
}
