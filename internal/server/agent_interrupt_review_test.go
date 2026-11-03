package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/model"
)

func TestManagedInterruptFailureDefersFinalCost(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(j.Managed.SessionID, "running", "", 100)
	ma.SetError("POST", "/v1/sessions/"+j.Managed.SessionID+"/events", 503)
	ma.SetError("POST", "/v1/sessions/"+j.Managed.SessionID+"/archive", 503)
	_ = f.db.Update(func(st *model.State) error {
		x := st.Jobs[j.ID]
		x.Managed.Deadline = time.Now().Add(-time.Second)
		st.Jobs[x.ID] = x
		return nil
	})
	err := f.s.reconcileAgentJob(context.Background(), j)
	if err != nil && !errors.Is(err, managedagents.ErrTemporary) {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Jobs[j.ID].Status != "running" {
			t.Error("stopped tracking running session")
		}
		if st.Jobs[j.ID].Managed.ListCostCents != 0 {
			t.Error("partial cost recorded")
		}
		return nil
	})
	restartManagedFixture(t, f)
	ma.SetError("POST", "/v1/sessions/"+j.Managed.SessionID+"/events", 0)
	ma.SetError("POST", "/v1/sessions/"+j.Managed.SessionID+"/archive", 0)
	ma.SetStatus(j.Managed.SessionID, "idle", "end_turn", 200)
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" {
		t.Fatal(j)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Jobs[j.ID].Managed.ListCostCents != 200 {
			t.Error("final usage lost")
		}
		return nil
	})
	assertAgentCleanup(t, f, ma, j)
}
