package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner/claudemanaged"
	"github.com/mash/kioku/internal/runner/claudemanaged/fake"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/store"
)

type fixture struct {
	auto         *fake.Server
	worker       bool
	runnerConfig runners.Config
	t            *testing.T
	s            *Server
	url          string
	client       *http.Client
	stateStore   *store.Database
}

func setup(t *testing.T) *fixture { return setupWithWorker(t, true) }
func setupWithWorker(t *testing.T, startWorker bool) *fixture {
	t.Helper()
	dir := t.TempDir()
	stateStore, err := openFixtureDatabase(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	// Lifecycle tests queue jobs explicitly, including fixtures that start a worker later.
	s := New(Config{Prompts: defaultPrompts(t), GenerateDebounce: time.Hour, BaseURL: "http://127.0.0.1", Providers: map[string]auth.Provider{}}, stateStore, local.Store{Dir: dir + "/objects"}, key)
	s.Config.Providers["google"] = auth.Provider{Name: "google", Client: &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		uid := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var u model.User
		_ = stateStore.Read(context.Background(), func(st *store.Queries) error { u = fixtureUsersGet(t, st, uid); return nil })
		body, _ := json.Marshal(map[string]any{"sub": uid, "name": u.Name, "email": u.Email, "email_verified": true})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}}
	ts := httptest.NewServer(s.Handler())
	s.Config.BaseURL = ts.URL
	f := &fixture{t: t, s: s, url: ts.URL, client: ts.Client(), stateStore: stateStore, worker: startWorker}
	if startWorker {
		configureAutoRunner(t, f)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	if startWorker {
		go func() { done <- s.RunWorker(ctx) }()
	} else {
		done <- nil
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		ts.Close()
		stateStore.Close()
	})
	return f
}
func (f *fixture) request(method, path, token string, in, out any) int {
	t := f.t
	f.t.Helper()
	// Test callers use internal IDs to arrange state. Public requests use names.
	path = f.publicPath(path)
	var b []byte
	if in != nil {
		b, _ = json.Marshal(in)
	}
	req, err := http.NewRequest(method, f.url+path, bytes.NewReader(b))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Kioku-Client-Version", "dev")
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	if out != nil {
		if err = json.Unmarshal(raw, out); err != nil {
			f.t.Fatalf("decode %s: %v: %s", path, err, raw)
		}
	}
	if resp.StatusCode < 300 {
		_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
			switch v := out.(type) {
			case *fixtureProject:
				parts := strings.Split(v.Namespace, "/")
				if len(parts) == 2 {
					org, err := st.OrganizationBySlug(parts[0])
					fixtureCheck(t, err)
					row, err := st.ProjectBySlug(org.ID, parts[1])
					fixtureCheck(t, err)
					*v = fixtureProjectsGet(t, st, row.ID)
				}
			case *fixtureJob:
				if j, ok := fixtureJobsFind(t, st, v.ID); ok {
					*v = j
				}
			}
			return nil
		})
	}
	return resp.StatusCode
}
func (f *fixture) login(email string) string {
	t := f.t
	f.t.Helper()
	token := auth.Token()
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		uid := ""
		users, err := st.UsersByEmail(email)
		if err != nil {
			return err
		}
		if len(users) != 0 {
			uid = users[0].ID
		}
		if uid == "" {
			uid = auth.Token()[:20]
			slug := defaultUserSlug(email)
			now := time.Now().UnixMilli()
			taken, err := st.OrganizationSlugTaken(slug, now)
			if err != nil {
				return err
			}
			if taken {
				slug = "user-" + auth.Hash(email)[:12]
			}
			if err := createAccount(st, uid, slug, email, strings.Split(email, "@")[0], now); err != nil {
				return err
			}
			if err := st.PutIdentity(uid, "google", uid); err != nil {
				return err
			}
		}
		access := f.s.Cipher.SealIdentityTokens(auth.Credential{AccessToken: uid}, uid, "google", uid)
		fixtureSessionsPut(t, st, auth.Hash(token), fixtureSession{UserID: uid, Expires: time.Now().Add(time.Hour), IdentityProvider: "google", IdentityID: uid, IdentityAccessToken: store.ValueString(access.AccessToken)})
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	return token
}

func (f *fixture) wait(pid, token, id string) fixtureJob {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var j fixtureJob
		if code := f.request("GET", "/api/projects/"+pid+"/jobs/"+id, token, nil, &j); code != 200 {
			f.t.Fatalf("job status %d", code)
		}
		if f.auto != nil && j.Status == "running" {
			current := storedJob(f.t, f, id)
			if current.ViewGeneration != nil {
				f.driveViewGeneration(current)
			} else if !agentRunFinished(fixtureRow(current)) && current.Run != nil && current.Run.Ref.ID != "" {
				if current.Run.OutputHash == "" {
					sendOutput(f.t, f, f.auto, current, "Generated")
				}
				f.auto.SetStatus(current.Run.Ref.ID, "idle", "end_turn", 1)
				f.s.ReconcileJob(context.Background(), current.ID)
			}
		}
		if j.Status == "failed" {
			f.t.Fatal(j.Error)
		}
		if j.Status == "completed" {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatal("job timed out")
	return fixtureJob{}
}

// Run one synthetic View attempt for tests that focus on the Wiki stage.
func (f *fixture) driveViewGeneration(parent fixtureJob) {
	t := f.t
	f.t.Helper()
	ctx := f.t.Context()
	if !f.worker {
		if err := f.s.storage.advanceViews(ctx, parent.ID); err != nil {
			f.t.Fatal(err)
		}
	}
	parent = storedJob(f.t, f, parent.ID)
	if parent.ViewGeneration == nil || parent.ViewGeneration.ChildID == "" {
		return
	}
	child := storedJob(f.t, f, parent.ViewGeneration.ChildID)
	if !f.worker && child.Status == "queued" {
		child.Status = "queued"
		if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
			fixtureJobsPut(t, st, child.ID, child)
			return nil
		}); err != nil {
			f.t.Fatal(err)
		}
		if err := f.s.startAgentJob(ctx, fixtureRow(child)); err != nil {
			f.t.Fatal(err)
		}
		child = storedJob(f.t, f, child.ID)
	}
	if f.auto == nil || child.Status != "running" || child.Run == nil || child.Run.Ref.ID == "" {
		return
	}
	if child.Run.OutputHash == "" {
		body := outputArchive(f.t, map[string]string{"index.html": "<h1>Project map</h1>"})
		f.auto.SetOutput(child.Run.Ref.ID, body)
	}
	f.auto.SetStatus(child.Run.Ref.ID, "idle", "end_turn", 1)
	if err := f.s.reconcileAgentJob(ctx, fixtureRow(child)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) completeFixtureViews(parent fixtureJob) fixtureJob {
	f.t.Helper()
	if parent.ViewGeneration != nil && (f.worker || f.auto == nil) {
		f.t.Fatal("completeFixtureViews requires a fake runner without a background worker")
	}
	for parent.ViewGeneration != nil {
		f.driveViewGeneration(parent)
		if err := f.s.storage.advanceViews(f.t.Context(), parent.ID); err != nil {
			f.t.Fatal(err)
		}
		next := storedJob(f.t, f, parent.ID)
		if next.ViewGeneration != nil && next.ViewGeneration.Next <= parent.ViewGeneration.Next {
			f.t.Fatalf("View generation did not advance: parent=%s next=%d child=%q", parent.ID, next.ViewGeneration.Next, next.ViewGeneration.ChildID)
		}
		parent = next
	}
	return parent
}

// generate queues wiki generation explicitly for waiting inbox items.
func (f *fixture) generate(pid, token string, _ bool) fixtureJob {
	f.t.Helper()
	var job fixtureJob
	if code := f.request("POST", "/api/projects/"+pid+"/generate", token, nil, &job); code != 202 {
		f.t.Fatalf("generate: %d", code)
	}
	return job
}

func TestProjectSharingPutAndIsolation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	alice, bob, eve := f.login("alice@example.com"), f.login("bob@example.com"), f.login("eve@example.com")
	var p fixtureProject
	if code := f.request("POST", "/api/projects", alice, map[string]string{"name": "Shared context"}, &p); code != 201 {
		t.Fatal(code)
	}
	base := "/api/projects/" + p.ID
	if code := f.request("GET", base+"/members", eve, nil, nil); code != 404 {
		t.Fatalf("nonmember list: %d", code)
	}
	var inv struct {
		URL string `json:"url"`
	}
	if code := f.request("POST", base+"/invites", alice, map[string]string{"email": "bob@example.com"}, &inv); code != 201 {
		t.Fatal(code)
	}
	if code := f.request("POST", "/api/join", eve, map[string]string{"token": inv.URL}, nil); code != 403 {
		t.Fatalf("email-bound invite: %d", code)
	}
	if code := f.request("POST", "/api/join", bob, map[string]string{"token": inv.URL}, nil); code != 200 {
		t.Fatal(code)
	}
	if code := f.request("POST", "/api/join", bob, map[string]string{"token": inv.URL}, nil); code != 410 {
		t.Fatalf("single use: %d", code)
	}
	if code := f.request("POST", base+"/invites", bob, map[string]string{}, nil); code != 403 {
		t.Fatalf("member cannot invite: %d", code)
	}
	req := model.PutRequest{File: &model.FileInput{Name: "decisions.md", Content: testSourceContent(testSourceURL("decisions.md", "# Decision\nWe use Go and S3."), "# Decision\nWe use Go and S3.")}}
	if code := f.request("POST", base+"/put", eve, req, nil); code != 404 {
		t.Fatalf("nonmember put: %d", code)
	}
	if code := f.request("POST", base+"/put", bob, req, nil); code != 202 {
		t.Fatal(code)
	}
	first := waitingInboxItem(t, f, p.ID, testSourceURL("decisions.md", "# Decision\nWe use Go and S3."))
	f.wait(p.ID, bob, f.generate(p.ID, bob, false).ID)
	wiki := f.readWiki(base, alice)
	if !strings.Contains(wiki[sourcePage(first.ID)], "Go and S3") {
		t.Fatal("source not in wiki")
	}
	var projects []fixtureProjectEntry
	if code := f.request("GET", "/api/projects", eve, nil, &projects); code != 200 || len(projects) != 1 || !projects[0].General {
		t.Fatalf("project and inbox isolation: %d %+v", code, projects)
	}
	resp, err := f.client.Get(f.url + "/api/projects")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal(resp.StatusCode)
	}
	if code := f.request("DELETE", base+"/members/bob@example.com", alice, nil, nil); code != 200 {
		t.Fatal(code)
	}
	if code := f.request("GET", base+"/snapshot?manifest=1", bob, nil, nil); code != 404 {
		t.Fatalf("revocation: %d", code)
	}
	if code := f.request("DELETE", base+"/members/alice@example.com", alice, nil, nil); code != 400 {
		t.Fatalf("owner removal: %d", code)
	}
	if code := f.request("POST", base+"/put", alice, req, nil); code != 202 {
		t.Fatal(code)
	}
	second := waitingInboxItem(t, f, p.ID, first.SourceURL)
	f.wait(p.ID, alice, f.generate(p.ID, alice, true).ID)
	if second.ID == first.ID {
		t.Fatal("put reused an inbox ID")
	}
	if !strings.Contains(f.readWiki(base, alice)[sourcePage(first.ID)], "Go and S3") {
		t.Fatal("updated source not in wiki")
	}
}
func TestConcurrentSubmissionsAreNotLost(t *testing.T) {
	t.Parallel()
	f := setup(t)
	token := f.login("owner@example.com")
	var p fixtureProject
	f.request("POST", "/api/projects", token, map[string]string{"name": "Concurrent"}, &p)
	base := "/api/projects/" + p.ID
	items := make(chan model.InboxItem, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			items <- putFile(t, f, token, p.ID, fmt.Sprintf("note-%d.md", i), fmt.Sprintf("Unique note %d", i))
		}(i)
	}
	wg.Wait()
	close(items)
	for j := range items {
		if j.ID == "" {
			t.Fatal("missing inbox ID")
		}
	}
	f.wait(p.ID, token, f.generate(p.ID, token, false).ID)
	count := 0
	for name := range f.readWiki(base, token) {
		if strings.HasPrefix(name, "wiki/source-") {
			count++
		}
	}
	if count != 8 {
		t.Fatalf("expected 8 source pages, got %d", count)
	}
}
func TestCSRFAndOAuthState(t *testing.T) {
	t.Parallel()
	f := setup(t)
	token := f.login("owner@example.com")
	req, _ := http.NewRequest("POST", f.url+"/api/projects", strings.NewReader(`{"name":"bad"}`))
	req.AddCookie(&http.Cookie{Name: "kioku_session", Value: token})
	req.Header.Set("Origin", "https://attacker.example")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("cross origin allowed")
	}
	if code := f.request("GET", "/auth/google/callback?state=bad&code=bad", token, nil, nil); code != 403 {
		t.Fatalf("OAuth state check: %d", code)
	}
	var secrets string
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error { b, _ := json.Marshal(st); secrets = string(b); return nil })
	if strings.Contains(secrets, token) {
		t.Fatal("session token stored in plaintext")
	}
}

func TestNoUnauthenticatedEmailLogin(t *testing.T) {
	t.Parallel()
	f := setup(t)
	code := f.request("POST", "/api/auth/dev", "", map[string]string{"email": "owner@example.com"}, nil)
	if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Fatalf("removed login endpoint returned %d", code)
	}
	if code = f.request("GET", "/api/me", "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request returned %d", code)
	}
}

func (f *fixture) setRunners() {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.runnerConfig.Prompts = f.s.Config.Prompts
	g, err := runners.Load(f.runnerConfig, func(jobID string) { f.s.ReconcileJob(ctx, jobID) })
	if err != nil {
		f.t.Fatal(err)
	}
	f.s.SetRunner(g.Runner)
	f.t.Cleanup(func() {
		cancel()
		if err := g.Close(context.Background()); err != nil {
			f.t.Error(err)
		}
	})
}

func configureAutoRunner(t *testing.T, f *fixture) {
	t.Helper()
	ma := fake.New()
	t.Cleanup(ma.Close)
	f.auto = ma
	f.runnerConfig = runners.Config{Prompts: f.s.Config.Prompts, AnthropicAPIKey: "fake-key", MAAgentID: "agent_test", MAEnvironmentID: "env_test", MABaseURL: ma.URL(), ManagedAgents: claudemanaged.NewAPI(ma.URL(), "fake-key", ma.HTTP.Client()), BaseURL: f.s.Config.BaseURL}
	f.setRunners()
}
func sourcePage(id string) string {
	return "wiki/source-" + strings.ReplaceAll(strings.ToLower(id), "_", "-") + ".md"
}

// publicPath resolves internal fixture references before sending a real request.
func (f *fixture) publicPath(path string) string {
	t := f.t
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		parts := strings.Split(path, "/")
		index := 2
		if strings.HasPrefix(path, "/api/projects/") {
			index = 3
		}
		if len(parts) <= index {
			return nil
		}
		id := strings.SplitN(parts[index], "?", 2)[0]
		p, ok := fixtureProjectsFind(t, st, id)
		if ok {
			path = strings.Replace(path, "/"+id, "/"+p.Namespace, 1)
		}

		return nil
	})
	return path
}
