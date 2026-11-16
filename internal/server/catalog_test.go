package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"sync/atomic"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

// The fault changes only the commit response; it never repeats the mutation.
type uncertainCatalog struct {
	catalog
	armed               atomic.Bool
	apply, failRecovery bool
	after               func()
	writes, reconnects  atomic.Int32
}

func (d *uncertainCatalog) Write(ctx context.Context, fn func(*store.Queries) error) error {
	if !d.armed.Swap(false) {
		return d.catalog.Write(ctx, fn)
	}
	d.writes.Add(1)
	rollback := errors.New("test rollback before lost response")
	err := d.catalog.Write(ctx, func(q *store.Queries) error {
		if err := fn(q); err != nil {
			return err
		}
		if !d.apply {
			return rollback
		}
		return nil
	})
	if err != nil && !errors.Is(err, rollback) {
		return err
	}
	if d.after != nil {
		d.after()
	}
	return &store.UnknownCommit{Cause: errors.New("lost commit response")}
}
func (d *uncertainCatalog) ResolveCommit(ctx context.Context, fn func(*store.Queries) error) error {
	d.reconnects.Add(1)
	if d.failRecovery {
		return &store.FatalDatabaseError{Cause: errors.New("test connection unavailable")}
	}
	return d.catalog.ResolveCommit(ctx, fn)
}
func installUncertainCatalog(f *fixture, apply bool) *uncertainCatalog {
	d := &uncertainCatalog{catalog: f.stateStore, apply: apply}
	d.armed.Store(true)
	f.s.Store = d
	f.s.storage.db = d
	return d
}
func TestInboxUnknownCommitResolvesWithoutRepeatingMutation(t *testing.T) {
	for _, which := range []string{"applied", "not applied", "revoked", "fatal"} {
		t.Run(which, func(t *testing.T) {
			f, token, p := orgProject(t)
			d := installUncertainCatalog(f, which == "applied")
			d.failRecovery = which == "fatal"
			if which == "revoked" {
				d.after = func() {
					fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error { return q.DeleteSession(auth.Hash(token)) }))
				}
			}
			req := model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://example.com/unknown", "Evidence")}}
			want := map[string]int{"applied": 202, "not applied": 503, "revoked": 401, "fatal": 500}[which]
			if code := f.request("POST", "/api/projects/"+p.Namespace+"/put", token, req, nil); code != want {
				t.Fatal(code, want)
			}
			if d.writes.Load() != 1 || d.reconnects.Load() != 1 {
				t.Fatal("write/reconnect repeated", d.writes.Load(), d.reconnects.Load())
			}
			items := fixtureInboxList(t, f, p.ID)
			if (len(items) == 1) != (which == "applied") {
				t.Fatal(items)
			}
			keys, err := f.s.Blobs.List(t.Context(), "projects/"+p.ID+"/.kioku/inbox/")
			fixtureCheck(t, err)
			if len(keys) != 1 {
				t.Fatal(keys)
			}
			if which == "not applied" {
				if code := f.request("POST", "/api/projects/"+p.Namespace+"/put", token, req, nil); code != 202 {
					t.Fatal(code)
				}
				items = fixtureInboxList(t, f, p.ID)
				if len(items) != 1 {
					t.Fatal(items)
				}
				for _, item := range items {
					if item.BodyKey() == keys[0] {
						t.Fatal("retry reused the ambiguous item ID")
					}
				}
			}
			if which == "fatal" {
				if err := f.s.RunWorker(t.Context()); err == nil {
					t.Fatal("worker continued after fatal reconnect")
				}
				var health map[string]string
				if c := f.request("GET", "/healthz", "", nil, &health); c != 500 || health["error"] != "internal server error" {
					t.Fatal("fatal catalog health contract", c, health)
				}
			}
		})
	}
}
func TestCLIExchangeUnknownCommitRequiresNewLogin(t *testing.T) {
	for _, applied := range []bool{true, false} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			f := setupLoopback(t)
			a := newLoopbackAttempt(t, f)
			callback := finishLoopbackOAuth(t, f, startLoopbackOAuth(t, f, a))
			defer callback.Body.Close()
			target, err := callback.Location()
			fixtureCheck(t, err)
			d := installUncertainCatalog(f, applied)
			var response map[string]any
			req := map[string]string{"device": a.device, "code": target.Query().Get("code"), "code_verifier": a.verifier}
			if code := f.request("POST", "/api/auth/loopback/token", "", req, &response); code != 410 {
				t.Fatal(code, response)
			}
			if _, ok := response["token"]; ok {
				t.Fatal("secret returned after unknown commit")
			}
			if d.reconnects.Load() != 1 || d.writes.Load() != 1 {
				t.Fatal("exchange repeated")
			}
			if n := sessionKindCount(t, f, "cli"); n != map[bool]int{true: 1, false: 0}[applied] {
				t.Fatal(n)
			}
		})
	}
}
func TestOAuthSQLFailureRollsBackCodeExchange(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	client := connectorClient(t, f)
	_, form := connectorAuthCode(t, f, client, human, p.ID)
	restore := fixtureSQLFault(t, f, "sessions", "INSERT", "NEW.kind='mcp'")
	if response := connectorForm(f, "POST", "/oauth/token", "", form); response.Code != 500 {
		t.Fatal("SQL failure disguised as protocol rejection", response.Code, response.Body.String())
	}
	restore()
	if response := connectorForm(f, "POST", "/oauth/token", "", form); response.Code != 200 {
		t.Fatal("SQL failure consumed code", response.Code, response.Body.String())
	}
}
