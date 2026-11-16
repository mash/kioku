package server

import (
	"context"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func TestManagedOldFailureCannotStopNewAttempt(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	old := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(old.Run.Ref.ID, "idle", "retries_exhausted", 100)
	reconcileManaged(t, f, old)
	next := startManaged(t, f, token, p.ID, false)
	if err := f.s.failAgentJobLocked(context.Background(), fixtureRow(old), "late failure", nil); err != nil {
		t.Fatal(err)
	}
	got := storedJob(t, f, next.ID)
	if got.Status != "running" || got.Run.AbortReason != "" || got.Run.Attempt.ID != next.Run.Attempt.ID {
		t.Fatal("old failure changed new run", got)
	}

	if ma.Session(next.Run.Ref.ID).Deleted {
		t.Error("new session archived")
	}
}
func TestManagedPreparingGracePreservesUnknownCreation(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	ma.SetError("POST", "/v1/sessions", 503)
	j := startManaged(t, f, token, p.ID, false)
	before := len(ma.Requests())
	j = reconcileManaged(t, f, j)
	if j.Status != "running" || j.Run.Phase != model.PhasePreparing || len(ma.Requests()) != before+1 {
		t.Fatal("creation grace skipped", j)
	}

	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		x := fixtureJobsGet(t, st, j.ID)
		x.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
		fixtureJobsPut(t, st, j.ID, x)
		return nil
	})
	if got := reconcileManaged(t, f, j); got.Status != "failed" {
		t.Fatal("expired creation not recovered", got)
	}
}
