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
	if err := f.s.Blobs.Put(context.Background(), j.Managed.OutputKey, outputArchive(t, map[string]string{"wiki/index.md": "Replaced"})); err != nil {
		t.Fatal(err)
	}
	ma.SetStatus(j.Managed.SessionID, "idle", "end_turn", 123)
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
	ma.SetStatus(old.Managed.SessionID, "idle", "retries_exhausted", 100)
	reconcileManaged(t, f, old)
	next := retryManagedJob(t, f, token, old)
	if err := f.s.failAgentJob(context.Background(), old, "late failure"); err != nil {
		t.Fatal(err)
	}
	got := storedJob(t, f, next.ID)
	if got.Status != "running" || got.Managed.AbortReason != "" || got.Managed.Attempt.ID != next.Managed.Attempt.ID {
		t.Fatal("old failure changed new run", got)
	}
	if c, b := agentRequest(t, f, next, ma.Credential(next.Managed.VaultID), "GET", "input", nil); c != 200 {
		t.Fatal("new credential disabled", c, string(b))
	}
	if ma.Session(next.Managed.SessionID).Archived {
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
	if j.Status != "running" || j.Managed.Phase != model.PhasePreparing || len(ma.Requests()) != before {
		t.Fatal("creation grace skipped", j)
	}
	if c, _ := agentRequest(t, f, j, ma.Credential(j.Managed.VaultID), "GET", "input", nil); c != 200 {
		t.Fatal("credential expired during grace", c)
	}
	_ = f.db.Update(func(st *model.State) error {
		x := st.Jobs[j.ID]
		x.Managed.PreparedAt = time.Now().Add(-6 * time.Minute)
		st.Jobs[j.ID] = x
		return nil
	})
	if got := reconcileManaged(t, f, j); got.Status != "failed" {
		t.Fatal("expired creation not recovered", got)
	}
}
