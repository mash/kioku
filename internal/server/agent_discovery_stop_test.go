package server

import (
	"context"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func TestManagedDiscoveryDefersStopUntilInspectionRecovers(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	id := j.Run.Ref.ID
	ma.SetStatus(id, "running", "", 123)
	ma.SetError("GET", "/v1/sessions/"+id, 503)
	_ = f.db.Update(func(st *model.State) error {
		x := st.Jobs[j.ID]
		x.Run.Ref.ID = ""
		x.Run.Phase = model.PhasePreparing
		x.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
		x.Run.Deadline = time.Now().Add(-time.Second)
		st.Jobs[x.ID] = x
		return nil
	})
	for range 2 {
		if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if ma.Session(id).Interrupted {
		t.Error("inspection error must not supply a RunRef")
	}
	ma.SetError("GET", "/v1/sessions/"+id, 0)
	if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ma.Session(id).Interrupted {
		t.Fatal("recovered run was not stopped")
	}
	if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
}
