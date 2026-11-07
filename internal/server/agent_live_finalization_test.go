package server

import (
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestManagedLiveSessionCannotPublish(t *testing.T) {
	for _, status := range []string{"running", "rescheduling"} {
		t.Run(status, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			putFile(t, f, token, p.ID, "x.md", "evidence")
			j := startManaged(t, f, token, p.ID, false)
			sendOutput(t, f, ma, j, "Final result")
			setManagedPhase(t, f, j, model.PhaseFinalizing, "running")
			ma.SetStatus(j.Run.Ref.ID, status, "", 100)
			j = reconcileManaged(t, f, j)
			if j.Status != "running" {
				t.Fatal("live session finalized", j.Status)
			}
			_ = f.db.View(func(st model.State) error {
				if costOf(st.Jobs[j.ID].Run.CostCents) != 0 {
					t.Error("interim cost recorded")
				}
				if st.Projects[p.ID].Revision != "" {
					t.Error("published before stop")
				}
				return nil
			})
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 200)
			j = reconcileManaged(t, f, j)
			if j.Status != "completed" {
				t.Fatal(j)
			}
			_ = f.db.View(func(st model.State) error {
				if costOf(st.Jobs[j.ID].Run.CostCents) != 200 {
					t.Error("final cost not recorded")
				}
				return nil
			})
		})
	}
}
