package server

import (
	"context"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func TestManagedDiscoveryDefersStopUntilInspectionRecovers(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	id := j.Run.Ref.ID
	ma.SetStatus(id, "running", "", 123)
	ma.SetError("GET", "/v1/sessions/"+id, 503)
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		x := fixtureJobsGet(t, st, j.ID)
		x.Run.Ref.ID = ""
		x.Run.Phase = model.PhasePreparing
		x.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
		x.Run.Deadline = time.Now().Add(-time.Second)
		fixtureJobsPut(t, st, x.ID, x)
		return nil
	})
	for range 2 {
		if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// E14: save the stop reason before invoking Stop; Stop may discover the session independently.
	got := storedJob(t, f, j.ID)
	if got.Run.Ref.ID != "" || got.Run.AbortReason == "" || got.Run.CostCents != nil {
		t.Fatal(got.Run)
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
