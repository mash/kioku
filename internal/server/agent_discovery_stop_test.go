package server

import (
	"context"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func TestManagedDiscoveredSessionStopsWithoutDetailLookup(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	id := j.Managed.SessionID
	ma.SetStatus(id, "running", "", 123)
	ma.SetError("GET", "/v1/sessions/"+id, 503)
	_ = f.db.Update(func(st *model.State) error {
		x := st.Jobs[j.ID]
		x.Managed.SessionID = ""
		x.Managed.Phase = model.PhasePreparing
		x.Managed.PreparedAt = time.Now().Add(-6 * time.Minute)
		x.Managed.Deadline = time.Now().Add(-time.Second)
		st.Jobs[x.ID] = x
		return nil
	})
	for range 2 {
		if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if !ma.Session(id).Interrupted {
		t.Error("detail lookup prevented interrupt of discovered session")
	}
	ma.SetError("GET", "/v1/sessions/"+id, 0)
	if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
}
