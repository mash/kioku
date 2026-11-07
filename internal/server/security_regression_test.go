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

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

func securityProject(t *testing.T, f *fixture, token string) model.Project {
	t.Helper()
	var p model.Project
	if c := f.request("POST", "/api/projects", token, map[string]string{"name": "Security regression"}, &p); c != 201 {
		t.Fatal(c)
	}
	return p
}
func TestEmailInviteUsesCurrentProviderProof(t *testing.T) {
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
	f.request("POST", "/api/projects/"+p.ID+"/invites", owner, map[string]string{"email": "former@company.example"}, &invite)
	provider := f.s.Config.Providers["google"]
	provider.Client = &http.Client{Transport: oauthTransport(func(*http.Request) (*http.Response, error) {
		b, _ := json.Marshal(map[string]any{"sub": user.ID, "name": "Current user", "email": "new@elsewhere.example", "email_verified": true})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})}
	f.s.Config.Providers["google"] = provider
	if c := f.request("POST", "/api/join", token, map[string]string{"token": invite.URL}, nil); c != 403 {
		t.Fatalf("stale email invitation accepted: %d", c)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Projects[p.ID].Members[user.ID] != "" {
			t.Error("unauthorized membership")
		}
		if st.Users[user.ID].Email != "new@elsewhere.example" {
			t.Error("profile email was not refreshed")
		}
		return nil
	})
	f.request("POST", "/api/projects/"+p.ID+"/invites", owner, map[string]string{"email": "new@elsewhere.example"}, &invite)
	if c := f.request("POST", "/api/join", token, map[string]string{"token": invite.URL}, nil); c != 200 {
		t.Fatalf("current email denied: %d", c)
	}
}
func TestEmailInviteRejectsMissingProofAndProviderFailure(t *testing.T) {
	for _, mode := range []string{"legacy session", "provider failure", "wrong identity", "unverified email"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			owner := f.login("owner@example.com")
			token := f.login("member@example.com")
			p := securityProject(t, f, owner)
			var invite struct {
				URL string `json:"url"`
			}
			f.request("POST", "/api/projects/"+p.ID+"/invites", owner, map[string]string{"email": "member@example.com"}, &invite)
			expected := 502
			if mode == "legacy session" {
				if err := f.db.Update(func(st *model.State) error {
					for k, v := range st.Sessions {
						if st.Users[v.UserID].Email == "member@example.com" {
							v.IdentityCredential = ""
							st.Sessions[k] = v
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
			_ = f.db.View(func(st model.State) error {
				for _, inv := range st.Invites {
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
	f := setup(t)
	owner := f.login("owner@example.com")
	attacker := f.login("attacker@example.com")
	p := securityProject(t, f, owner)
	unlock, err := f.s.lockProject(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	body := &pausedReader{make(chan struct{}), make(chan struct{})}
	defer close(body.release)
	req := httptest.NewRequest("POST", "/api/projects/"+p.ID+"/maintenance", body)
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
	f := setup(t)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	body := &pausedReader{make(chan struct{}), make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(body.release) }) }
	defer release()
	req := httptest.NewRequest("POST", "/api/projects/"+p.ID+"/maintenance", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Kioku-Client-Version", "dev")
	done := make(chan struct{})
	go func() { f.s.Handler().ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	<-body.started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err := f.s.lockProject(ctx, p.ID)
	if err != nil {
		t.Fatal("slow body held gate", err)
	}
	unlock()
	release()
	<-done
}
func TestProjectGatesAreIndependentAndCancelable(t *testing.T) {
	f := setup(t)
	unlock, err := f.s.lockProject(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	other, err := f.s.lockProject(ctx, "two")
	if err != nil {
		t.Fatal("other project blocked", err)
	}
	other()
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err = f.s.lockProject(canceled, "one"); !errors.Is(err, context.Canceled) {
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
		database <- f.db.Update(func(st *model.State) error {
			u := st.Users
			for id, user := range u {
				user.Name = "Updated while response is blocked"
				st.Users[id] = user
			}
			return nil
		})
	}()
	select {
	case err := <-database:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow client blocked global DB")
	}
	release()
	<-done
}

func TestRejectedUploadsWriteNoObjects(t *testing.T) {
	f := setup(t)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	f.s.Config.MaxBytes = 1
	for i := 0; i < 3; i++ {
		if c := f.seedSource("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "over-quota.md", Content: "Rejected", SourceURL: testSourceURL("over-quota.md", "Rejected")}}, nil); c != 413 {
			t.Fatal(c)
		}
	}
	keys, err := f.s.Blobs.List(context.Background(), "projects/"+p.ID+"/")
	if err != nil || len(nonStateObjects(keys)) != 0 {
		t.Fatal("rejected upload wrote objects", keys, err)
	}
	_ = f.db.View(func(st model.State) error {
		if len(st.Uploads) != 0 || len(st.Sources) != 0 || len(st.Jobs) != 0 {
			t.Error("rejected upload left metadata")
		}
		return nil
	})
}

type failingUploadStore struct {
	blob.Store
	mu         sync.Mutex
	failDelete bool
	putCount   int
}

func (b *failingUploadStore) Put(ctx context.Context, key blob.Key, data []byte) error {
	b.mu.Lock()
	b.putCount++
	b.mu.Unlock()
	if err := b.Store.Put(ctx, key, data); err != nil {
		return err
	}
	return errors.New("simulated lost storage response after commit")
}
func (b *failingUploadStore) Delete(ctx context.Context, key blob.Key) error {
	b.mu.Lock()
	fail := b.failDelete
	b.mu.Unlock()
	if fail {
		return errors.New("simulated delete outage")
	}
	return b.Store.Delete(ctx, key)
}
func TestFailedUploadsAreCleanedOrRemainReserved(t *testing.T) {
	for _, outage := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup works", true: "cleanup unavailable"}[outage], func(t *testing.T) {
			f := setup(t)
			token := f.login("owner@example.com")
			p := securityProject(t, f, token)
			objects := &failingUploadStore{Store: f.s.Blobs, failDelete: outage}
			f.s.Blobs = objects
			if c := f.seedSource("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: "Evidence", SourceURL: testSourceURL("source.md", "Evidence")}}, nil); c != 500 {
				t.Fatal(c)
			}
			var reserved int
			_ = f.db.View(func(st model.State) error {
				reserved = len(st.Uploads)
				if len(st.Sources) != 0 || len(st.Jobs) != 0 {
					t.Error("failed upload was published")
				}
				return nil
			})
			want := 0
			if outage {
				want = 1
			}
			if reserved != want {
				t.Fatal("reservation lost", reserved)
			}
			if outage {
				objects.mu.Lock()
				objects.failDelete = false
				objects.mu.Unlock()
				unlock, err := f.s.lockProject(context.Background(), p.ID)
				if err != nil {
					t.Fatal(err)
				}
				err = f.s.recoverProjectUploads(context.Background(), p.ID)
				unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			keys, err := objects.List(context.Background(), "projects/"+p.ID+"/")
			if err != nil || len(nonStateObjects(keys)) != 0 {
				t.Fatal("orphan objects", keys, err)
			}
		})
	}
}
func TestInterruptedUploadRecoveryAndGC(t *testing.T) {
	f := setup(t)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	unlock, err := f.s.lockProject(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	src := model.Source{ID: "interrupted", ProjectID: p.ID, Bytes: 40, TextKey: blob.Key("projects/" + p.ID + "/sources/interrupted/text")}
	if err = f.db.Update(func(st *model.State) error {
		st.Uploads["interrupted"] = model.Upload{ID: "interrupted", ProjectID: p.ID, Sources: []model.Source{src}, Job: model.Job{ID: "j", ProjectID: p.ID}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Blobs.Put(context.Background(), src.TextKey, []byte("partially uploaded")); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	if n, err := f.s.gc(req, p.ID); err != nil || n != 0 {
		t.Fatal("GC removed a reserved object", n, err)
	}
	if err = f.s.recoverProjectUploads(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	_ = f.db.View(func(st model.State) error {
		if len(st.Uploads) != 0 {
			t.Error("reservation not reclaimed")
		}
		return nil
	})
	keys, err := f.s.Blobs.List(context.Background(), "projects/"+p.ID+"/")
	if err != nil || len(nonStateObjects(keys)) != 0 {
		t.Fatal("recovery left objects", keys, err)
	}
}
func TestFullQueueRejectsBeforeWritingObjects(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	f.s.Config.MaxQueued = 1
	// A running job consumes a slot even before its revision is published.
	unlock, err := f.s.lockProject(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.Update(func(st *model.State) error {
		st.Jobs["already-running"] = model.Job{ID: "already-running", ProjectID: p.ID, UserID: "irrelevant", Status: "running"}
		return nil
	}); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if err = f.db.Update(func(st *model.State) error {
		st.Jobs["failed"] = model.Job{ID: "failed", ProjectID: p.ID, Status: "failed"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/jobs/failed/retry", token, nil, nil); c != 429 {
		t.Fatal("retry bypassed queue capacity", c)
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/generate", token, nil, nil); c != 429 {
		t.Fatal("full queue was not rejected", c)
	}
	keys, err := f.s.Blobs.List(context.Background(), "projects/"+p.ID+"/")
	if err != nil || len(nonStateObjects(keys)) != 0 {
		t.Fatal("queue rejection wrote objects", keys, err)
	}
}

func TestWorkerRecoversUnfinishedUploadOnStartup(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	src := model.Source{ID: "unfinished", ProjectID: p.ID, Bytes: 40, TextKey: blob.Key("projects/" + p.ID + "/sources/unfinished/text")}
	if err := f.db.Update(func(st *model.State) error {
		st.Uploads["unfinished"] = model.Upload{ID: "unfinished", ProjectID: p.ID, Sources: []model.Source{src}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Blobs.Put(context.Background(), src.TextKey, []byte("partially uploaded")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.s.RunWorker(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		pending := 0
		if err := f.db.View(func(st model.State) error { pending = len(st.Uploads); return nil }); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup did not reclaim reservation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	keys, err := f.s.Blobs.List(context.Background(), "projects/"+p.ID+"/")
	if err != nil || len(nonStateObjects(keys)) != 0 {
		t.Fatal("startup left untracked objects", keys, err)
	}
}
func TestRecoverySkipsUploadsStillInProgress(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	unlock, err := f.s.lockProject(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err = f.db.Update(func(st *model.State) error {
		st.Uploads["active"] = model.Upload{ID: "active", ProjectID: p.ID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = f.s.recoverUploads(ctx); err != nil {
		t.Fatal("active request blocked worker recovery", err)
	}
	_ = f.db.View(func(st model.State) error {
		if _, ok := st.Uploads["active"]; !ok {
			t.Error("recovery discarded active upload")
		}
		return nil
	})
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
