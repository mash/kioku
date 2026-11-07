package server

import (
	"context"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func TestManagedStoredOutputHashRejectsReplacement(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Original")
	j = storedJob(t, f, j.ID)
	if err := f.s.Blobs.Put(context.Background(), j.Run.OutputKey, outputArchive(t, map[string]string{"wiki/index.md": "Replaced"})); err != nil {
		t.Fatal(err)
	}
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" || j.Error != "agent output hash mismatch" {
		t.Fatal("tampered output accepted", j.Status, j.Error)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Projects[p.ID].Revision != "" {
			t.Error("tampered output published")
		}
		return nil
	})
}
func TestManagedOldFailureCannotStopNewAttempt(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	old := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(old.Run.Ref.ID, "idle", "retries_exhausted", 100)
	reconcileManaged(t, f, old)
	next := retryManagedJob(t, f, token, old)
	if err := f.s.failAgentJobLocked(context.Background(), old, "late failure", nil); err != nil {
		t.Fatal(err)
	}
	got := storedJob(t, f, next.ID)
	if got.Status != "running" || got.Run.AbortReason != "" || got.Run.Attempt.ID != next.Run.Attempt.ID {
		t.Fatal("old failure changed new run", got)
	}
	if c, b := agentRequest(t, f, next, ma.Credential(next.Run.Ref.Resources["vault_id"]), "GET", "input", nil); c != 200 {
		t.Fatal("new credential disabled", c, string(b))
	}
	if ma.Session(next.Run.Ref.ID).Archived {
		t.Error("new session archived")
	}
}
func TestManagedPreparingGracePreservesUnknownCreation(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	ma.SetError("POST", "/v1/sessions", 503)
	j := startManaged(t, f, token, p.ID, false)
	before := len(ma.Requests())
	j = reconcileManaged(t, f, j)
	if j.Status != "running" || j.Run.Phase != model.PhasePreparing || len(ma.Requests()) != before+1 {
		t.Fatal("creation grace skipped", j)
	}
	if c, _ := agentRequest(t, f, j, ma.Credential("vault_1"), "GET", "input", nil); c != 200 {
		t.Fatal("credential expired during grace", c)
	}
	_ = f.db.Update(func(st *model.State) error {
		x := st.Jobs[j.ID]
		x.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
		st.Jobs[j.ID] = x
		return nil
	})
	if got := reconcileManaged(t, f, j); got.Status != "failed" {
		t.Fatal("expired creation not recovered", got)
	}
}
