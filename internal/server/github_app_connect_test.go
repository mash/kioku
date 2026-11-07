package server

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/githubapp"
	"github.com/mash/kioku/internal/model"
)

func appFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := setupWithWorker(t, false)
	token := f.login("app-owner@example.com")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	config := githubapp.Config{Enabled: true, ID: 7, ClientID: "Iv.fixture", Slug: "test-app", ClientSecret: "client-secret", PrivateKey: key}
	f.s.Config.GitHubApp = config
	f.s.GitHubApp.Client.Config = config
	f.s.GitHubApp.Client.HTTP = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		body := ""
		switch r.URL.Path {
		case "/login/oauth/access_token":
			body = `{"access_token":"app-user-secret","refresh_token":"app-refresh-secret","expires_in":28800,"refresh_token_expires_in":15000000}`
		case "/user":
			body = `{"id":42,"login":"fixture-user"}`
		case "/user/installations":
			body = `{"installations":[{"id":3,"app_id":7,"account":{"id":9,"login":"acme"}}]}`
		case "/app/installations/3":
			body = `{"id":3,"app_id":7,"account":{"id":9,"login":"acme"},"permissions":{"contents":"read","metadata":"read","issues":"read"}}`
		case "/user/installations/3/repositories":
			body = `{"repositories":[{"id":101,"full_name":"acme/docs"}]}`
		default:
			t.Errorf("unexpected GitHub request: %s", r.URL.Path)
			return nil, errors.New("unexpected endpoint")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return f, token
}
func appRequest(f *fixture, method, path string, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", f.s.Config.BaseURL)
	}
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	return w
}
func startApp(t *testing.T, f *fixture, token string) (loopbackAttempt, *http.Cookie, string) {
	t.Helper()
	a := loopbackAttempt{verifier: auth.Token(), state: auth.Token(), redirect: "http://127.0.0.1:5000/callback"}
	var out map[string]string
	if c := f.request("POST", "/api/connect/github", token, map[string]string{"provider": "github", "code_challenge": auth.Challenge(a.verifier), "code_challenge_method": "S256", "redirect_uri": a.redirect, "state": a.state}, &out); c != 201 {
		t.Fatal(c)
	}
	a.device = out["device"]
	w := appRequest(f, "GET", out["url"], nil, nil)
	if w.Code != 302 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Path != "/auth/github-app/" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal(cookies)
	}
	target, _ := url.Parse(w.Header().Get("Location"))
	if target.Query().Get("scope") != "" || target.Query().Get("client_id") != "Iv.fixture" {
		t.Fatal("OAuth login configuration reused")
	}
	return a, cookies[0], target.Query().Get("state")
}
func stageApp(t *testing.T, f *fixture, token string) (loopbackAttempt, string) {
	t.Helper()
	a, cookie, state := startApp(t, f, token)
	w := appRequest(f, "GET", "/auth/github-app/callback?code=fixture&state="+state, cookie, nil)
	if w.Code != 302 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = appRequest(f, "GET", "/auth/github-app/installations", cookie, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(m) != 2 {
		t.Fatal(w.Body.String())
	}
	w = appRequest(f, "POST", "/auth/github-app/installations", cookie, url.Values{"csrf": {m[1]}, "installation": {"3"}})
	if w.Code != 302 {
		t.Fatal(w.Code, w.Body.String())
	}
	target, _ := url.Parse(w.Header().Get("Location"))
	if target.Query().Get("state") != a.state {
		t.Fatal("lost loopback state")
	}
	return a, target.Query().Get("code")
}
func TestGitHubAppConnectUsesExactSessionAndDoesNotChangeLogin(t *testing.T) {
	for _, mode := range []string{"success", "other session", "other owner", "wrong verifier", "expired", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			f, token := appFixture(t)
			before := loopbackSnapshot(t, f)
			a, code := stageApp(t, f, token)
			staged := loopbackSnapshot(t, f)
			if !reflect.DeepEqual(before.Users, staged.Users) || !reflect.DeepEqual(before.Sessions, staged.Sessions) {
				t.Fatal("callback changed login")
			}
			uid := before.Sessions[auth.Hash(token)].UserID
			req := connectExchange(a, code)
			want := 200
			switch mode {
			case "other session":
				token = auth.Token()
				_ = f.db.Update(func(st *model.State) error {
					for _, session := range before.Sessions {
						st.Sessions[auth.Hash(token)] = session
					}
					return nil
				})
				want = 403
			case "other owner":
				token = f.login("other-owner@example.com")
				want = 403
			case "wrong verifier":
				req["code_verifier"] = auth.Token()
				want = 410
			case "expired":
				_ = f.db.Update(func(st *model.State) error {
					d := st.Devices[a.device]
					d.Expires = time.Now().Add(-time.Second)
					st.Devices[a.device] = d
					return nil
				})
				want = 410
			case "revoked":
				_ = f.db.Update(func(st *model.State) error { delete(st.Sessions, auth.Hash(token)); return nil })
				want = 401
			}
			if c := f.request("POST", "/api/connect/github/complete", token, req, nil); c != want {
				t.Fatal(c, want)
			}
			_ = f.db.View(func(st model.State) error {
				if len(st.GitHubAppConnectAttempts) != 0 {
					t.Fatal("staging retained")
				}
				connection := st.GitHubAppConnections[uid]
				if want == 200 {
					if connection.Status != "active" || connection.GitHubUserID != 42 || len(connection.Installations) != 1 {
						t.Fatal(connection.Status)
					}
					if !reflect.DeepEqual(before.Users, st.Users) {
						t.Fatal("login mutated")
					}
					raw, err := f.s.Cipher.Open(connection.EncryptedCredential, "github-app:"+uid)
					if err != nil || !strings.Contains(raw, "app-user-secret") {
						t.Fatal("missing encrypted user token")
					}
				} else if connection.Status != "" {
					t.Fatal("bad completion connected")
				}
				return nil
			})
			if want == 200 {
				var account, me map[string]any
				f.request("GET", "/api/account", token, nil, &account)
				f.request("GET", "/api/me", token, nil, &me)
				if !reflect.DeepEqual(account["connections"], me["connections"]) {
					t.Fatal("projection differs")
				}
				b, _ := json.Marshal(account)
				if strings.Contains(string(b), "secret") {
					t.Fatal("credential in account")
				}
				if c := f.request("POST", "/api/connect/github/complete", token, req, nil); c != 410 {
					t.Fatal("reused code", c)
				}
			}
		})
	}
}
func TestGitHubAppConnectionVersionWinsOnlyOnce(t *testing.T) {
	f, token := appFixture(t)
	a, codeA := stageApp(t, f, token)
	b, codeB := stageApp(t, f, token)
	if c := f.request("POST", "/api/connect/github/complete", token, connectExchange(b, codeB), nil); c != 200 {
		t.Fatal(c)
	}
	if c := f.request("POST", "/api/connect/github/complete", token, connectExchange(a, codeA), nil); c != 409 {
		t.Fatal(c)
	}
}
func TestGitHubAppBrowserForgeryAndSetup(t *testing.T) {
	f, token := appFixture(t)
	_, cookie, state := startApp(t, f, token)
	for _, tc := range []struct {
		cookie *http.Cookie
		state  string
	}{{nil, state}, {cookie, "wrong"}, {&http.Cookie{Name: cookie.Name, Value: cookie.Value + "wrong"}, state}} {
		if w := appRequest(f, "GET", "/auth/github-app/callback?code=fixture&state="+tc.state, tc.cookie, nil); w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	if w := appRequest(f, "GET", "/auth/github-app/callback?code=fixture&state="+state, cookie, nil); w.Code != 302 {
		t.Fatal(w.Code)
	}
	if w := appRequest(f, "GET", "/auth/github-app/callback?code=fixture&state="+state, cookie, nil); w.Code != 403 {
		t.Fatal("state reused")
	}
	w := appRequest(f, "GET", "/auth/github-app/installations", cookie, nil)
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(m) != 2 {
		t.Fatal(w.Body.String())
	}
	if w := appRequest(f, "POST", "/auth/github-app/installations", cookie, url.Values{"csrf": {"wrong"}, "installation": {"3"}}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := appRequest(f, "POST", "/auth/github-app/installations", cookie, url.Values{"csrf": {m[1]}, "installation": {"999"}}); w.Code != 403 {
		t.Fatal("forged installation", w.Code)
	}
	if w := appRequest(f, "GET", "/auth/github-app/setup?installation_id=3&state=wrong", cookie, nil); w.Code != 403 {
		t.Fatal("trusted setup query", w.Code)
	}
}
func TestGitHubLegacyConnectNeverRequestsRepo(t *testing.T) {
	f, token := appFixture(t)
	if err := f.s.Prepare(); err != nil {
		t.Fatal(err)
	}
	_ = f.db.Update(func(st *model.State) error {
		st.Devices["legacy"] = model.Device{Purpose: "connect", IdentityProvider: "github", UserID: st.Sessions[auth.Hash(token)].UserID, LoopbackRedirectURI: "http://127.0.0.1:1234/callback", LoopbackState: "bound", Expires: time.Now().Add(time.Minute)}
		return nil
	})
	w := appRequest(f, "GET", "/auth/github/start?connect=legacy", nil, nil)
	if w.Code != 302 || !strings.Contains(w.Header().Get("Location"), "restart_required") {
		t.Fatal(w.Code)
	}
	f.s.Config.GitHubApp.Enabled = false
	if c := f.request("POST", "/api/connect/github", token, map[string]string{}, nil); c != 503 {
		t.Fatal(c)
	}
}
