package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

func TestGitHubAppLoginKeepsIdentityAndSourceAccessSeparate(t *testing.T) {
	for _, mode := range []string{"browser", "cli"} {
		t.Run(mode, func(t *testing.T) {
			f, existing := appFixture(t)
			if err := f.s.Prepare(); err != nil {
				t.Fatal(err)
			}
			var uid string
			if err := f.db.Update(func(st *model.State) error {
				uid = st.Sessions[auth.Hash(existing)].UserID
				u := st.Users[uid]
				u.Identities["github"] = "42"
				st.Users[uid] = u
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			app := f.s.Config.GitHubApp
			challenge := ""
			p := auth.Provider{Name: "github", ClientID: app.ClientID, ClientSecret: string(app.ClientSecret), RedirectURL: f.url + "/auth/github/callback"}
			p.Client = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
				body := ""
				switch r.URL.Host + r.URL.Path {
				case "github.com/login/oauth/access_token":
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if r.Form.Get("client_id") != app.ClientID || r.Form.Get("client_secret") != string(app.ClientSecret) || r.Form.Get("redirect_uri") != p.RedirectURL || auth.Challenge(r.Form.Get("code_verifier")) != challenge {
						t.Fatal("App credentials, callback or PKCE mismatch")
					}
					body = `{"access_token":"ghu_login_only","refresh_token":"ghr_login_only","expires_in":28800}`
				case "api.github.com/user":
					body = `{"id":42,"login":"fixture-user"}`
				case "api.github.com/user/emails":
					body = `[{"email":"verified@example.com","primary":true,"verified":true}]`
				default:
					t.Fatalf("login credential used outside identity APIs: %s", r.URL)
				}
				if r.URL.Host == "api.github.com" && r.Header.Get("Authorization") != "Bearer ghu_login_only" {
					t.Fatal("wrong identity token")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			f.s.Config.Providers["github"] = p
			start := "/auth/github/start"
			var device map[string]string
			verifier := auth.Token()
			if mode == "cli" {
				if code := f.request("POST", "/api/auth/loopback", "", map[string]string{"provider": "github", "code_challenge": auth.Challenge(verifier), "code_challenge_method": "S256", "redirect_uri": "http://127.0.0.1:5000/callback", "state": "cli-state"}, &device); code != 201 {
					t.Fatal(code)
				}
				start = device["url"]
			}
			w := appRequest(f, "GET", start, nil, nil)
			if w.Code != 302 {
				t.Fatal(w.Code)
			}
			u, err := url.Parse(w.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			challenge = q.Get("code_challenge")
			if q.Get("client_id") != app.ClientID || q.Get("redirect_uri") != p.RedirectURL || q.Has("scope") || q.Get("state") == "" || challenge == "" || q.Get("code_challenge_method") != "S256" {
				t.Fatal("incorrect App login authorization")
			}
			cookies := w.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Name != "kioku_oauth" || cookies[0].Path != "/auth/" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
				t.Fatal("lost login CSRF cookie")
			}
			query := "?state=" + url.QueryEscape(q.Get("state")) + "&code=login-code"
			if bad := appRequest(f, "GET", "/auth/github/callback"+query, nil, nil); bad.Code != 403 {
				t.Fatal("accepted missing login cookie")
			}
			if bad := appRequest(f, "GET", "/auth/github-app/callback"+query, cookies[0], nil); bad.Code != 403 {
				t.Fatal("login completed a source authorization")
			}
			w = appRequest(f, "GET", "/auth/github/callback"+query, cookies[0], nil)
			var token string
			if mode == "cli" {
				if w.Code != 302 {
					t.Fatal(w.Code, w.Body.String())
				}
				redirect, err := url.Parse(w.Header().Get("Location"))
				if err != nil || redirect.Query().Get("state") != "cli-state" {
					t.Fatal("lost CLI state")
				}
				var result map[string]string
				request := map[string]string{"device": device["device"], "code": redirect.Query().Get("code"), "code_verifier": verifier}
				if code := f.request("POST", "/api/auth/loopback/token", "", request, &result); code != 200 {
					t.Fatal(code)
				}
				token = result["token"]
				if code := f.request("POST", "/api/auth/loopback/token", "", request, nil); code != 410 {
					t.Fatal("reused CLI code")
				}
			} else {
				if w.Code != 303 {
					t.Fatal(w.Code, w.Body.String())
				}
				for _, c := range w.Result().Cookies() {
					if c.Name == "kioku_session" {
						token = c.Value
					}
				}
			}
			if token == "" {
				t.Fatal("missing session")
			}
			if replay := appRequest(f, "GET", "/auth/github/callback"+query, cookies[0], nil); replay.Code != 403 {
				t.Fatal("reused OAuth state")
			}
			if code := f.request("GET", "/api/account", token, nil, nil); code != 200 {
				t.Fatal("App identity proof failed", code)
			}
			st := loopbackSnapshot(t, f)
			session := st.Sessions[auth.Hash(token)]
			if len(st.Users) != 1 || session.UserID != uid || session.IdentityProvider != "github" || session.IdentityID != "42" || st.Users[uid].Email != "verified@example.com" {
				t.Fatal("numeric identity no longer matched")
			}
			plain, err := f.s.Cipher.Open(session.IdentityCredential, identityContext(uid, "github", "42"))
			if err != nil || !strings.Contains(plain, "ghu_login_only") {
				t.Fatal("missing isolated identity proof")
			}
			if len(st.Users[uid].Connections) != 0 || len(st.GitHubAppConnections) != 0 || len(st.GitHubAppConnectAttempts) != 0 || len(st.GitHubTokenAttempts) != 0 {
				t.Fatal("login persisted source credentials")
			}
			creds, selections, err := f.s.githubCredentials(t.Context(), uid, runner.AttemptKey{}, 0, []GitHubRepository{{Account: "acme", Name: "docs"}})
			if err != nil || len(creds) != 0 || len(selections) != 1 || selections[0].Reason != "not_connected" {
				t.Fatal("login token became a source credential", err)
			}
		})
	}
}

func TestGitHubLoginUnavailableWithoutConfiguration(t *testing.T) {
	f := setupLoopback(t)
	if w := appRequest(f, "GET", "/auth/github/start", nil, nil); w.Code != 400 {
		t.Fatal("unconfigured GitHub offered browser login")
	}
	if code := f.request("POST", "/api/auth/loopback", "", map[string]string{"provider": "github", "code_challenge": auth.Challenge(auth.Token()), "code_challenge_method": "S256", "redirect_uri": "http://127.0.0.1:5000/callback", "state": "cli-state"}, nil); code != 400 {
		t.Fatal("unconfigured GitHub offered CLI login")
	}
	a := newLoopbackAttempt(t, f)
	resp := finishLoopbackOAuth(t, f, startLoopbackOAuth(t, f, a))
	if resp.StatusCode != 302 {
		t.Fatal("Google login without the App failed", resp.StatusCode)
	}
}
