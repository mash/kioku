package server

import (
	"context"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestManagedDeleteRecoversUnrecordedSession(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	put := putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(j.Run.Ref.ID, "running", "", 123)
	_ = f.db.Update(func(st *model.State) error {
		x := st.Jobs[j.ID]
		x.Run.Ref.ID = ""
		x.Run.Phase = model.PhasePreparing
		st.Jobs[x.ID] = x
		return nil
	})
	path := "/api/projects/" + p.ID + "/sources/" + put.SourceIDs[0]
	if c := f.request("DELETE", path, token, nil, nil); c != 503 {
		t.Fatal(c)
	}
	for range 2 {
		if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if c := f.request("DELETE", path, token, nil, nil); c != 202 {
		t.Fatal(c)
	}
	if !ma.Session(j.Run.Ref.ID).Interrupted || !ma.Session(j.Run.Ref.ID).Archived {
		t.Error("unrecorded session orphaned")
	}
}

func TestMissingSourceDeletionLeavesGenerationRunning(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	if c := f.request("DELETE", "/api/projects/"+p.ID+"/sources/missing", token, nil, nil); c != 404 {
		t.Fatal(c)
	}
	if got := storedJob(t, f, j.ID); got.Status != "running" {
		t.Error("404 canceled generation", got.Status)
	}
	if ma.Session(j.Run.Ref.ID).Interrupted {
		t.Error("404 interrupted session")
	}
}
