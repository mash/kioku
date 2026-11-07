package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

// Signal each lock wait without relying on scheduler timing or sleeps.
type githubLockContext struct {
	context.Context
	waits chan struct{}
}

func (c githubLockContext) Done() <-chan struct{} {
	c.waits <- struct{}{}
	return c.Context.Done()
}

func TestGitHubInvalidationLocksProjectsAcquiredDuringWait(t *testing.T) {
	f, token := appFixture(t)
	uid := loopbackSnapshot(t, f).Sessions[auth.Hash(token)].UserID
	if err := f.db.Update(func(st *model.State) error {
		st.Projects["a"] = model.Project{ID: "a", Members: map[string]string{uid: "owner"}}
		st.Projects["b"] = model.Project{ID: "b", Members: map[string]string{uid: "member", "other": "owner"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	unlockA, err := f.s.lockProject(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	unlockB, err := f.s.lockProject(t.Context(), "b")
	if err != nil {
		t.Fatal(err)
	}
	defer unlockB()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	waits := make(chan struct{}, 10)
	r := httptest.NewRequest("POST", "/api/connect/github/complete", nil).WithContext(githubLockContext{Context: ctx, waits: waits})
	r.Header.Set("Authorization", "Bearer "+token)
	done := make(chan error, 1)
	go func() {
		done <- f.s.updateAccount(r, uid, func(st *model.State) error {
			disconnectGitHub(st, uid)
			return nil
		})
	}()
	select {
	case <-waits:
	case <-ctx.Done():
		unlockA()
		t.Fatal("invalidation did not reach the publication lock")
	}
	if err := f.db.Update(func(st *model.State) error {
		p := st.Projects["b"]
		p.Members[uid], p.Members["other"] = "owner", "member"
		st.Projects["b"] = p
		return nil
	}); err != nil {
		unlockA()
		t.Fatal(err)
	}
	unlockA()
	// A fresh snapshot must acquire both a and b before invalidation can commit.
	for range 2 {
		select {
		case err := <-done:
			t.Fatalf("invalidation crossed b's publication lock: %v", err)
		case <-waits:
		case <-ctx.Done():
			t.Fatal("invalidation did not retry the ownership snapshot")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("invalidation should wait for b's publication: %v", err)
	}
	_ = f.db.View(func(st model.State) error {
		if st.GitHubAppConnections[uid].Version != 0 {
			t.Fatal("connection changed while publication was in progress")
		}
		return nil
	})
}
