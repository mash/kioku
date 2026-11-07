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
	session := ma.Session(j.Run.Ref.ID)
	session.Status = "terminated"
	session.CostAmount = ""
	ma.SetSession(session)
	if err := f.db.Update(func(st *model.State) error {
		current := st.Jobs[j.ID]
		current.Run.Deadline = time.Now().Add(-time.Minute)
		st.Jobs[j.ID] = current
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
	if err := f.db.View(func(st model.State) error {
		if costOf(st.Jobs[j.ID].Run.CostCents) != 0 {
			t.Error("unknown usage recorded a cost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
