package server

import (
	"context"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func retryManagedJob(t *testing.T, f *fixture, token string, j model.Job) model.Job {
	t.Helper()
	var next model.Job
	if c := f.request("POST", "/api/projects/"+j.ProjectID+"/jobs/"+j.ID+"/retry", token, nil, &next); c != 202 {
		t.Fatal(c)
	}
	if err := f.db.Update(func(st *model.State) error {
		next = st.Jobs[j.ID]
		next.Status = "running"
		st.Jobs[j.ID] = next
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.startAgentJob(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	return storedJob(t, f, j.ID)
}
func TestManagedRecoveryNeverAttributesPreviousAttempt(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	first := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(first.Run.Ref.ID, "idle", "retries_exhausted", 100)
	ma.SetError("POST", "/v1/sessions/"+first.Run.Ref.ID+"/archive", 503)
	reconcileManaged(t, f, first)
	oldSession := ma.Session(first.Run.Ref.ID)
	ma.SetError("POST", "/v1/sessions/"+first.Run.Ref.ID+"/archive", 0)
	reconcileManaged(t, f, first)
	if got := costOf(storedJob(t, f, first.ID).Run.CostCents); got != 100 {
		t.Fatal("first attempt cost lost", got)
	}
	ma.SetError("POST", "/v1/sessions", 503)
	second := retryManagedJob(t, f, token, first)
	// A stale list entry from a previous attempt must remain harmless even
	// after the prior attempt's cleanup was acknowledged.
	ma.SetSession(oldSession)
	if second.Run.Ref.ID != "" {
		t.Fatal(second.Run)
	}
	_ = f.db.Update(func(st *model.State) error {
		j := st.Jobs[second.ID]
		j.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
		st.Jobs[j.ID] = j
		return nil
	})
	reconcileManaged(t, f, second)
	_ = f.db.View(func(st model.State) error {
		j := st.Jobs[second.ID]
		if j.Run.Attempt.ID != second.Run.Attempt.ID || j.Run.Ref.ID == first.Run.Ref.ID || costOf(j.Run.CostCents) != 0 {
			t.Error("previous attempt's session or cost reached the retry", j.Run)
		}
		return nil
	})
}
