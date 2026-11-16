package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/store"
)

func securityProject(t *testing.T, f *fixture, token string) fixtureProject {
	t.Helper()
	var p fixtureProject
	if c := f.request("POST", "/api/projects", token, map[string]string{"name": "Security regression"}, &p); c != 201 {
		t.Fatal(c)
	}
	return p
}
func TestEmailInviteUsesCurrentProviderProof(t *testing.T) {
	t.Parallel()
	f := setup(t)
	owner := f.login("owner@example.com")
	token := f.login("former@company.example")
	p := securityProject(t, f, owner)
	var user struct {
		ID string `json:"id"`
	}
	f.request("GET", "/api/me", token, nil, &user)
	var invite struct {
		URL string `json:"url"`
	}
	f.request("POST", "/api/projects/"+p.Namespace+"/invites", owner, map[string]string{"email": "former@company.example"}, &invite)
	provider := f.s.Config.Providers["google"]
	provider.Client = &http.Client{Transport: oauthTransport(func(*http.Request) (*http.Response, error) {
		b, _ := json.Marshal(map[string]any{"sub": user.ID, "name": "Current user", "email": "new@elsewhere.example", "email_verified": true})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})}
	f.s.Config.Providers["google"] = provider
	if c := f.request("POST", "/api/join", token, map[string]string{"token": invite.URL}, nil); c != 403 {
		t.Fatalf("stale email invitation accepted: %d", c)
	}
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if fixtureProjectsGet(t, st, p.ID).Members[user.ID] != "" {
			t.Error("unauthorized membership")
		}
		if fixtureUsersGet(t, st, user.ID).Email != "new@elsewhere.example" {
			t.Error("profile email was not refreshed")
		}
		return nil
	})
	f.request("POST", "/api/projects/"+p.Namespace+"/invites", owner, map[string]string{"email": "new@elsewhere.example"}, &invite)
	if c := f.request("POST", "/api/join", token, map[string]string{"token": invite.URL}, nil); c != 200 {
		t.Fatalf("current email denied: %d", c)
	}
}
func TestEmailInviteRejectsMissingProofAndProviderFailure(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"legacy session", "provider failure", "wrong identity", "unverified email"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			owner := f.login("owner@example.com")
			token := f.login("member@example.com")
			p := securityProject(t, f, owner)
			var invite struct {
				URL string `json:"url"`
			}
			f.request("POST", "/api/projects/"+p.Namespace+"/invites", owner, map[string]string{"email": "member@example.com"}, &invite)
			expected := 502
			if mode == "legacy session" {
				if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
					for k, v := range fixtureSessionsList(t, st, fixtureSessionsGet(t, st, auth.Hash(token)).UserID) {
						if fixtureUsersGet(t, st, v.UserID).Email == "member@example.com" {
							v.IdentityAccessToken = ""
							fixtureSessionsPut(t, st, k, v)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				expected = 401
			} else {
				provider := f.s.Config.Providers["google"]
				provider.Client = &http.Client{Transport: oauthTransport(func(*http.Request) (*http.Response, error) {
					if mode == "provider failure" {
						return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
					}
					body := `{"sub":"different-subject","email":"member@example.com","email_verified":true}`
					if mode == "unverified email" {
						body = `{"sub":"anything","email":"member@example.com","email_verified":false}`
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				f.s.Config.Providers["google"] = provider
				if mode == "wrong identity" {
					expected = 401
				}
			}
			if c := f.request("POST", "/api/join", token, map[string]string{"token": invite.URL}, nil); c != expected {
				t.Fatal(mode, c)
			}
			_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
				for _, inv := range fixtureInvitesList(t, st, p.ID) {
					if inv.ProjectID == p.ID && inv.UsedBy != "" {
						t.Error("failed verification consumed invite")
					}
				}
				return nil
			})
		})
	}
}

type pausedReader struct{ started, release chan struct{} }

func (r *pausedReader) Read([]byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	<-r.release
	return 0, io.EOF
}
func (*pausedReader) Close() error { return nil }
func TestUnauthorizedMaintenanceDoesNotReadBodyOrAcquireGate(t *testing.T) {
	t.Parallel()
	f := setup(t)
	owner := f.login("owner@example.com")
	attacker := f.login("attacker@example.com")
	p := securityProject(t, f, owner)
	unlock, err := f.s.storage.lock(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	body := &pausedReader{make(chan struct{}), make(chan struct{})}
	defer close(body.release)
	req := httptest.NewRequest("POST", "/api/projects/"+p.Namespace+"/maintenance", body)
	req.Header.Set("Authorization", "Bearer "+attacker)
	req.Header.Set("Kioku-Client-Version", "dev")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { f.s.Handler().ServeHTTP(rec, req); close(done) }()
	select {
	case <-body.started:
		t.Fatal("unauthorized request read the body")
	case <-done:
		if rec.Code != 404 {
			t.Fatal(rec.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("unauthorized request waited for the project gate")
	}
}
func TestSlowMaintenanceBodyDoesNotHoldProjectGate(t *testing.T) {
	t.Parallel()
	f := setup(t)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	body := &pausedReader{make(chan struct{}), make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(body.release) }) }
	defer release()
	req := httptest.NewRequest("POST", "/api/projects/"+p.Namespace+"/maintenance", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Kioku-Client-Version", "dev")
	done := make(chan struct{})
	go func() { f.s.Handler().ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	<-body.started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err := f.s.storage.lock(ctx, p.ID)
	if err != nil {
		t.Fatal("slow body held gate", err)
	}
	unlock()
	release()
	<-done
}
func TestProjectGatesAreIndependentAndCancelable(t *testing.T) {
	t.Parallel()
	f := setup(t)
	unlock, err := f.s.storage.lock(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	other, err := f.s.storage.lock(ctx, "two")
	if err != nil {
		t.Fatal("other project blocked", err)
	}
	other()
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err = f.s.storage.lock(canceled, "one"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
}

type pausedWriter struct {
	header           http.Header
	started, release chan struct{}
}

func (w *pausedWriter) Header() http.Header { return w.header }
func (*pausedWriter) WriteHeader(int)       {}
func (w *pausedWriter) Write(b []byte) (int, error) {
	select {
	case <-w.started:
	default:
		close(w.started)
	}
	<-w.release
	return len(b), nil
}
func TestSlowHTTPResponseDoesNotHoldDatabaseMutex(t *testing.T) {
	t.Parallel()
	f := setup(t)
	token := f.login("owner@example.com")
	securityProject(t, f, token)
	w := &pausedWriter{make(http.Header), make(chan struct{}), make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(w.release) }) }
	defer release()
	req := httptest.NewRequest("GET", "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Kioku-Client-Version", "dev")
	done := make(chan struct{})
	go func() { f.s.Handler().ServeHTTP(w, req); close(done) }()
	<-w.started
	database := make(chan error, 1)
	go func() {
		database <- f.stateStore.Write(context.Background(), func(st *store.Queries) error {
			session := fixtureSessionsGet(t, st, auth.Hash(token))
			user := fixtureUsersGet(t, st, session.UserID)
			user.Name = "Updated while response is blocked"
			fixtureUsersPut(t, st, user.ID, user)
			return nil
		})
	}()
	select {
	case err := <-database:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow client blocked global Store")
	}
	release()
	<-done
}

func nonStateObjects(keys []blob.Key) []blob.Key {
	var out []blob.Key
	for _, key := range keys {
		if !strings.Contains(string(key), "/.kioku/") {
			out = append(out, key)
		}
	}
	return out
}
