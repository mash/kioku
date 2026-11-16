package server

import (
	"context"
	"testing"

	"github.com/mash/kioku/internal/store"
)

func TestManagedLiveSessionCannotPublish(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"running", "rescheduling"} {
		t.Run(status, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			putFile(t, f, token, p.ID, "x.md", "evidence")
			j := startManaged(t, f, token, p.ID, false)
			sendOutput(t, f, ma, j, "Final result")
			ma.SetStatus(j.Run.Ref.ID, status, "", 100)
			j = reconcileManaged(t, f, j)
			if j.Status != "running" {
				t.Fatal("live session finalized", j.Status)
			}
			_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
				if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 0 {
					t.Error("interim cost recorded")
				}
				if fixtureProjectsGet(t, st, p.ID).Revision != "" {
					t.Error("published before stop")
				}
				return nil
			})
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 200)
			j = reconcileManaged(t, f, j)
			if j.Status != "completed" {
				t.Fatal(j)
			}
			_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
				if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 200 {
					t.Error("final cost not recorded")
				}
				return nil
			})
		})
	}
}
