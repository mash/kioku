package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner/claudemanaged"
)

func TestManagedInterruptFailureDefersFinalCost(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(j.Run.Ref.ID, "running", "", 100)
	ma.SetError("POST", "/v1/sessions/"+j.Run.Ref.ID+"/events", 503)
	ma.SetError("POST", "/v1/sessions/"+j.Run.Ref.ID+"/archive", 503)
	_ = f.db.Update(func(st *model.State) error {
		x := st.Jobs[j.ID]
		x.Run.Deadline = time.Now().Add(-time.Second)
		st.Jobs[x.ID] = x
		return nil
	})
	err := f.s.reconcileAgentJob(context.Background(), j)
	if err != nil && !errors.Is(err, claudemanaged.ErrTemporary) {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Jobs[j.ID].Status != "running" {
			t.Error("stopped tracking running session")
		}
		if costOf(st.Jobs[j.ID].Run.CostCents) != 0 {
			t.Error("partial cost recorded")
		}
		return nil
	})
	restartManagedFixture(t, f)
	ma.SetError("POST", "/v1/sessions/"+j.Run.Ref.ID+"/events", 0)
	ma.SetError("POST", "/v1/sessions/"+j.Run.Ref.ID+"/archive", 0)
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 200)
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" {
		t.Fatal(j)
	}
	_ = f.db.View(func(st model.State) error {
		if costOf(st.Jobs[j.ID].Run.CostCents) != 200 {
			t.Error("final usage lost")
		}
		return nil
	})
	assertAgentCleanup(t, f, ma, j)
}
