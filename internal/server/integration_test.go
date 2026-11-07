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
	"github.com/mash/kioku/internal/store"
)

type fixture struct {
	t      *testing.T
	s      *Server
	url    string
	client *http.Client
	db     *store.Store
}

func setup(t *testing.T) *fixture { return setupWithWorker(t, true) }
func setupWithWorker(t *testing.T, startWorker bool) *fixture {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{BaseURL: "http://127.0.0.1", Providers: map[string]auth.Provider{}}, db, local.Store{Dir: dir + "/objects"}, key)
	s.Config.Providers["google"] = auth.Provider{Name: "google", Client: &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		uid := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var u model.User
		_ = db.View(func(st model.State) error { u = st.Users[uid]; return nil })
		body, _ := json.Marshal(map[string]any{"sub": uid, "name": u.Name, "email": u.Email, "email_verified": true})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}}
	ts := httptest.NewServer(s.Handler())
	s.Config.BaseURL = ts.URL
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
		db.Close()
	})
	return &fixture{t, s, ts.URL, ts.Client(), db}
}
func (f *fixture) request(method, path, token string, in, out any) int {
	f.t.Helper()
	var b []byte
	if in != nil {
		b, _ = json.Marshal(in)
	}
	req, err := http.NewRequest(method, f.url+path, bytes.NewReader(b))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
	return resp.StatusCode
}
func (f *fixture) login(email string) string {
	f.t.Helper()
	token := auth.Token()
	if err := f.db.Update(func(st *model.State) error {
		uid := ""
		for id, u := range st.Users {
			if u.Email == email {
				uid = id
				break
			}
		}
		if uid == "" {
			uid = auth.Token()[:20]
			st.Users[uid] = model.User{ID: uid, Name: strings.Split(email, "@")[0], Email: email, Identities: map[string]string{"google": uid}, Connections: map[string]string{}}
		}
		data, _ := json.Marshal(auth.Credential{AccessToken: uid})
		proof := f.s.Cipher.Seal(string(data), identityContext(uid, "google", uid))
		st.Sessions[auth.Hash(token)] = model.Session{UserID: uid, Expires: time.Now().Add(time.Hour), IdentityProvider: "google", IdentityID: uid, IdentityCredential: proof}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	return token
}

func (f *fixture) wait(pid, token, id string) model.Job {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var j model.Job
		if code := f.request("GET", "/api/projects/"+pid+"/jobs/"+id, token, nil, &j); code != 200 {
			f.t.Fatalf("job status %d", code)
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
	return model.Job{}
}

// generate queues wiki generation explicitly after source intake.
func (f *fixture) generate(pid, token string, full bool) model.Job {
	f.t.Helper()
	var job model.Job
	if code := f.request("POST", "/api/projects/"+pid+"/generate", token, map[string]bool{"full": full}, &job); code != 202 {
		f.t.Fatalf("generate: %d", code)
	}
	return job
}

func TestProjectSharingPutAndIsolation(t *testing.T) {
	f := setup(t)
	alice, bob, eve := f.login("alice@example.com"), f.login("bob@example.com"), f.login("eve@example.com")
	var p model.Project
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
	req := model.PutRequest{Files: []model.FileInput{{Name: "decisions.md", Content: "# Decision\nWe use Go and S3."}}}
	if code := f.request("POST", base+"/put", eve, req, nil); code != 404 {
		t.Fatalf("nonmember put: %d", code)
	}
	var job model.Job
	if code := f.request("POST", base+"/put", bob, req, &job); code != 202 {
		t.Fatal(code)
	}
	f.wait(p.ID, bob, f.generate(p.ID, bob, false).ID)
	var snap model.Snapshot
	if code := f.request("GET", base+"/snapshot", alice, nil, &snap); code != 200 {
		t.Fatal(code)
	}
	if !strings.Contains(snap.Files["wiki/source-"+job.SourceIDs[0]+".md"], "Go and S3") {
		t.Fatal("source not in wiki")
	}
	if !strings.Contains(snap.Files["maps/graph.json"], "links-to") {
		t.Fatal("map missing edges")
	}
	srcPath := base + "/sources/" + job.SourceIDs[0] + "/text"
	if code := f.request("GET", srcPath, eve, nil, nil); code != 404 {
		t.Fatalf("source isolation: %d", code)
	}
	resp, err := f.client.Get(f.url + srcPath)
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
	if code := f.request("GET", base+"/snapshot", bob, nil, nil); code != 404 {
		t.Fatalf("revocation: %d", code)
	}
	if code := f.request("DELETE", base+"/members/alice@example.com", alice, nil, nil); code != 400 {
		t.Fatalf("owner removal: %d", code)
	}
	var second model.Job
	if code := f.request("POST", base+"/put", alice, req, &second); code != 202 {
		t.Fatal(code)
	}
	f.wait(p.ID, alice, f.generate(p.ID, alice, true).ID)
	if second.SourceIDs[0] != job.SourceIDs[0] {
		t.Fatal("duplicate source not deduplicated")
	}
	if code := f.request("GET", base+"/snapshot?revision="+snap.Revision, alice, nil, nil); code != 404 {
		t.Fatal("old snapshots should not be retained", code)
	}
	if code := f.request("GET", base+"/snapshot", alice, nil, &snap); code != 200 {
		t.Fatal(code)
	}
}
func TestConcurrentSubmissionsAreNotLost(t *testing.T) {
	f := setup(t)
	token := f.login("owner@example.com")
	var p model.Project
	f.request("POST", "/api/projects", token, map[string]string{"name": "Concurrent"}, &p)
	base := "/api/projects/" + p.ID
	jobs := make(chan model.Job, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var j model.Job
			code := f.request("POST", base+"/put", token, model.PutRequest{Files: []model.FileInput{{Name: fmt.Sprintf("note-%d.md", i), Content: fmt.Sprintf("Unique note %d", i)}}}, &j)
			if code != 202 {
				t.Errorf("put %d", code)
			}
			jobs <- j
		}(i)
	}
	wg.Wait()
	close(jobs)
	for j := range jobs {
		f.wait(p.ID, token, j.ID)
	}
	f.wait(p.ID, token, f.generate(p.ID, token, false).ID)
	var snap model.Snapshot
	f.request("GET", base+"/snapshot", token, nil, &snap)
	count := 0
	for name := range snap.Files {
		if strings.HasPrefix(name, "wiki/source-") {
			count++
		}
	}
	if count != 8 {
		t.Fatalf("expected 8 source pages, got %d", count)
	}
}
func TestCSRFAndOAuthState(t *testing.T) {
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
	_ = f.db.View(func(st model.State) error { b, _ := json.Marshal(st); secrets = string(b); return nil })
	if strings.Contains(secrets, token) {
		t.Fatal("session token stored in plaintext")
	}
}

func TestNoUnauthenticatedEmailLogin(t *testing.T) {
	f := setup(t)
	code := f.request("POST", "/api/auth/dev", "", map[string]string{"email": "owner@example.com"}, nil)
	if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Fatalf("removed login endpoint returned %d", code)
	}
	if code = f.request("GET", "/api/me", "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request returned %d", code)
	}
}
