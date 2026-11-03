package server

import (
	"context"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func TestUnknownCostFailureStillStopsSessionAfterRecovery(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(j.Managed.SessionID, "running", "", 100)
	path := "/v1/sessions/" + j.Managed.SessionID
	ma.SetError("POST", path+"/events", 503)
	ma.SetError("GET", path, 503)
	ma.SetError("POST", path+"/archive", 503)
	_ = f.db.Update(func(st *model.State) error {
		x := st.Jobs[j.ID]
		x.Managed.Deadline = time.Now().Add(-time.Second)
		x.Created = time.Now().Add(-31 * 24 * time.Hour)
		st.Jobs[x.ID] = x
		return nil
	})
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" {
		t.Fatal("unknown usage did not fail at deadline", j.Status)
	}
	if code := f.request("POST", "/api/projects/"+p.ID+"/maintenance", token, map[string]int{"keep": 0}, nil); code != 200 {
		t.Fatal(code)
	}
	restartManagedFixture(t, f)
	ma.SetError("POST", path+"/events", 0)
	ma.SetError("GET", path, 0)
	ma.SetError("POST", path+"/archive", 0)
	ma.SetStatus(j.Managed.SessionID, "running", "", 200)
	if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ma.Session(j.Managed.SessionID).Interrupted || ma.Session(j.Managed.SessionID).Status == "running" {
		t.Error("unknown-cost failure abandoned stop intent")
	}
	assertAgentCleanup(t, f, ma, j)
}
