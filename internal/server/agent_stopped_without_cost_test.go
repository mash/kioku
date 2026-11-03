package server

import (
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func TestManagedStoppedWithoutCostCompletesCleanup(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	session := ma.Session(j.Managed.SessionID)
	session.Status = "terminated"
	session.CostAmount = ""
	ma.SetSession(session)
	if err := f.db.Update(func(st *model.State) error {
		current := st.Jobs[j.ID]
		current.Managed.Deadline = time.Now().Add(-time.Minute)
		st.Jobs[j.ID] = current
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	j = reconcileManaged(t, f, j)
	restartManagedFixture(t, f)
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" || j.Managed.StopPending || !j.Managed.CleanupComplete {
		t.Fatalf("stopped session retained: status=%s stop_pending=%v cleanup=%v", j.Status, j.Managed.StopPending, j.Managed.CleanupComplete)
	}
	assertAgentCleanup(t, f, ma, j)
	if err := f.db.View(func(st model.State) error {
		if st.Jobs[j.ID].Managed.ListCostCents != 0 {
			t.Error("unknown usage recorded a cost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
