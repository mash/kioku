package server

import (
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

func browserPage(t *testing.T, f *fixture, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "kioku_session", Value: token})
	}
	out := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(out, req)
	return out
}

func TestWikiBrowserAccess(t *testing.T) {
	f, owner, p := orgProject(t)
	outsider := f.login("outsider@example.com")
	expired := f.login("expired@example.com")
	key := blob.Key("projects/" + p.ID + "/wiki/index.md")
	if err := f.s.Blobs.Put(t.Context(), key, []byte("# Private wiki body\n[Topic](secret-topic.md)")); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Update(func(st *model.State) error {
		session := st.Sessions[auth.Hash(expired)]
		session.Expires = time.Now().Add(-time.Hour)
		st.Sessions[auth.Hash(expired)] = session
		project := st.Projects[p.ID]
		project.Name = "Private project name"
		project.Revision = "web-revision"
		st.Projects[p.ID] = project
		st.Revisions[project.Revision] = model.Revision{ID: project.Revision, ProjectID: p.ID, Files: map[string]blob.Key{"wiki/index.md": key, "wiki/secret-topic.md": key}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/acme/platform", "/projects/" + p.ID} {
		for _, tc := range []struct {
			name, token string
			status      int
		}{{"anonymous", "", 404}, {"expired", expired, 404}, {"outsider", outsider, 404}, {"member", owner, 200}} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				out := browserPage(t, f, path, tc.token)
				if out.Code != tc.status || out.Header().Get("Content-Type") != "text/html; charset=utf-8" {
					t.Fatalf("%d %v %s", out.Code, out.Header(), out.Body)
				}
				body := out.Body.String()
				if !strings.Contains(body, `aria-label="アカウント"`) || out.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("missing navigation or cache policy")
				}
				for _, secret := range []string{"Private wiki body", "Private project name", "secret-topic.md"} {
					if strings.Contains(body, secret) != (tc.status == 200) {
						t.Fatalf("unexpected disclosure/rendering of %s", secret)
					}
				}
				if tc.token == "" || tc.token == expired {
					if !strings.Contains(body, `class="login"`) {
						t.Fatal("missing login")
					}
				} else if !strings.Contains(body, "ログイン済み") {
					t.Fatal("missing authenticated header")
				}
			})
		}
		out := browserPage(t, f, path+"?file=missing.md", owner)
		if out.Code != 404 || !strings.Contains(out.Body.String(), "ページが見つかりません") {
			t.Fatal(out.Code, out.Body)
		}
	}
	for _, token := range []string{"", owner, outsider} {
		missing := browserPage(t, f, "/acme/missing", token)
		denied := browserPage(t, f, "/acme/platform", outsider)
		if missing.Code != 404 {
			t.Fatal(missing.Code)
		}
		if token != "" && missing.Body.String() != denied.Body.String() {
			t.Fatal("nonexistent and unauthorized pages differ")
		}
	}
	_, agent := testAgent(t, f, owner, p.ID, "read")
	req := httptest.NewRequest("GET", "/acme/platform", nil)
	req.Header.Set("Authorization", "Bearer "+agent)
	out := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(out, req)
	if out.Code != 403 {
		t.Fatal("agent gained browser access", out.Code)
	}
	for _, tc := range []struct {
		token string
		code  int
	}{{"", 401}, {outsider, 404}} {
		out := browserPage(t, f, "/api/projects/"+p.ID+"/snapshot", tc.token)
		if out.Code != tc.code || out.Header().Get("Content-Type") != "application/json; charset=utf-8" {
			t.Fatal("API changed", out.Code, out.Body)
		}
	}
	if err := f.s.Blobs.Delete(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if out := browserPage(t, f, "/acme/platform", owner); out.Code != 500 {
		t.Fatal("storage error hidden", out.Code)
	}
}

func TestLoginReturnPaths(t *testing.T) {
	for _, value := range []string{"", "/", "/oauth/authorize?client_id=x&state=s", "/acme/platform?file=wiki%2Ftopic.md&q=a%26b", "/u-abc/project", "/projects/AbC_d-123", "/oauth/wiki", "/oauth/token"} {
		if got, err := authorizeReturn(value); err != nil || got != value {
			t.Errorf("valid %q: %q %v", value, got, err)
		}
	}
	for _, value := range []string{"https://evil.test/acme/wiki", "//evil.test/acme/wiki", "/\\evil.test/wiki", "/acme/../wiki", "/acme/%2e%2e", "/%2facme/wiki", "/acme%2fwiki", "/api/me", "/auth/start", "/oauth/token/extra", "/invite/token", "/acme/wiki#x", "/acme/wiki#", "/acme/wiki%0a", "acme/wiki", "/projects/..", "/projects/a%5cb"} {
		if _, err := authorizeReturn(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestWikiLoginRoundTrip(t *testing.T) {
	f := setupLoopback(t)
	target := "/acme/platform?file=wiki%2Ftopic.md&q=a%26b"
	page := browserPage(t, f, target, "")
	loginURL := "/?return=" + url.QueryEscape(target)
	if !strings.Contains(page.Body.String(), loginURL) {
		t.Fatal("404 lost return URL", page.Body)
	}
	home := browserPage(t, f, loginURL, "")
	if !strings.Contains(home.Body.String(), `class="login" href="`+loginURL+`"`) {
		t.Fatal("header nested the return URL", home.Body)
	}
	start := "/auth/google/start?return=" + url.QueryEscape(target)
	providerLink := ""
	for _, match := range regexp.MustCompile(`href="([^"]+)"`).FindAllStringSubmatch(home.Body.String(), -1) {
		link := html.UnescapeString(match[1])
		if strings.HasPrefix(link, "/auth/google/start?") {
			providerLink = link
		}
	}
	parsed, err := url.Parse(providerLink)
	if err != nil || parsed.Query().Get("return") != target {
		t.Fatal("home lost return URL", home.Body)
	}
	req := startLoopbackOAuth(t, f, loopbackAttempt{startURL: f.url + start})
	resp := finishLoopbackOAuth(t, f, req)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != target {
		t.Fatal(resp.StatusCode, resp.Header)
	}
	var session string
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "kioku_session" {
			session = cookie.Value
		}
	}
	if session == "" {
		t.Fatal("missing session")
	}
	// Logging in alone must not grant membership in the requested project.
	if page := browserPage(t, f, target, session); page.Code != 404 || !strings.Contains(page.Body.String(), "ログイン済み") {
		t.Fatal(page.Code, page.Body)
	}
	for _, endpoint := range []string{"/?return=", "/auth/google/start?return="} {
		page := browserPage(t, f, endpoint+url.QueryEscape("//evil.test/path"), "")
		if page.Code != 400 {
			t.Fatal("unsafe return accepted", page.Code)
		}
	}
	// The callback must revalidate persisted state too.
	req = startLoopbackOAuth(t, f, loopbackAttempt{startURL: f.url + start})
	if err := f.db.Update(func(st *model.State) error {
		for key, pending := range st.OAuth {
			pending.ReturnPath = "//evil.test/path"
			st.OAuth[key] = pending
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resp = finishLoopbackOAuth(t, f, req)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
		t.Fatal(resp.StatusCode, string(b))
	}
}

func TestOAuthNamespaceLoginReturn(t *testing.T) {
	f := setupWithWorker(t, false)
	owner := f.login("owner@example.com")
	createTestOrg(t, f, owner, "oauth")
	var p model.Project
	if code := f.request("POST", "/api/projects", owner, map[string]string{"name": "oauth/wiki"}, &p); code != 201 {
		t.Fatal(code)
	}
	page := browserPage(t, f, "/oauth/wiki", "")
	if page.Code != 404 || !strings.Contains(page.Body.String(), `href="/?return=%2Foauth%2Fwiki"`) {
		t.Fatal(page.Code, page.Body)
	}
	if page := browserPage(t, f, "/oauth/wiki", owner); page.Code != 200 {
		t.Fatal(page.Code, page.Body)
	}
}
