package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestGitHubLoginRefreshSharedProof(t *testing.T) {
	for _, mode := range []string{"shared sessions", "resealed shared proof", "identity lookup failure", "concurrent requests", "revoked during refresh"} {
		t.Run(mode, func(t *testing.T) {
			f := setupWithWorker(t, false)
			token := f.login("old@example.com")
			sibling := auth.Token()
			var uid, original, unrelated string
			if err := f.db.Update(func(st *model.State) error {
				s := st.Sessions[auth.Hash(token)]
				uid = s.UserID
				u := st.Users[uid]
				u.Identities["github"] = "42"
				st.Users[uid] = u
				data, _ := json.Marshal(auth.Credential{AccessToken: "ghu_old", RefreshToken: "ghr_old", ExpiresAt: time.Now().Add(-time.Minute)})
				original = f.s.Cipher.Seal(string(data), identityContext(uid, "github", "42"))
				s.IdentityProvider, s.IdentityID, s.IdentityCredential = "github", "42", original
				st.Sessions[auth.Hash(token)] = s
				if mode == "resealed shared proof" {
					// Older servers re-encrypted a proof after each identity lookup,
					// even when the access and refresh tokens had not changed.
					s.IdentityCredential = f.s.Cipher.Seal(string(data), identityContext(uid, "github", "42"))
				}
				st.Sessions[auth.Hash(sibling)] = s
				st.Devices["pending-cli"] = model.Device{UserID: uid, IdentityProvider: "github", IdentityID: "42", IdentityCredential: original, Expires: time.Now().Add(time.Minute)}
				unrelated = f.s.Cipher.Seal(`{"access_token":"another-login"}`, identityContext(uid, "github", "42"))
				s.IdentityCredential = unrelated
				st.Sessions["unrelated-login"] = s
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			refreshes, lookups := 0, 0
			f.s.Config.Providers["github"] = auth.Provider{Name: "github", ClientID: "Iv.fixture", ClientSecret: "secret", Client: &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				status, body := 200, ""
				switch r.URL.Host + r.URL.Path {
				case "github.com/login/oauth/access_token":
					if err := r.ParseForm(); err != nil {
						return nil, err
					}
					if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "ghr_old" || r.Form.Get("client_id") != "Iv.fixture" || r.Form.Get("client_secret") != "secret" {
						return nil, fmt.Errorf("incorrect refresh request")
					}
					refreshes++
					if refreshes > 1 {
						body = `{"error":"bad_refresh_token"}`
					} else {
						body = `{"access_token":"ghu_fresh","refresh_token":"ghr_fresh","expires_in":28800}`
					}
					if mode == "revoked during refresh" {
						if err := f.db.Update(func(st *model.State) error { delete(st.Sessions, auth.Hash(token)); return nil }); err != nil {
							return nil, err
						}
					}
				case "api.github.com/user":
					if r.Header.Get("Authorization") != "Bearer ghu_fresh" {
						return nil, fmt.Errorf("used stale login access token")
					}
					body = `{"id":42,"login":"fixture-user"}`
					lookups++
					if lookups == 1 && mode == "identity lookup failure" {
						status = 503
					}
				case "api.github.com/user/emails":
					body = `[{"email":"current@example.com","primary":true,"verified":true}]`
				default:
					return nil, fmt.Errorf("unexpected identity request: %s", r.URL)
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			verify := func(token string) error {
				r := httptest.NewRequest("POST", "/api/account/sessions/revoke-all", nil)
				r.Header.Set("Authorization", "Bearer "+token)
				// Middleware captures the session before waiting for the refresh lock.
				key, principal := f.s.session(r)
				r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, requestIdentity{key: key, principal: principal}))
				email, err := f.s.currentEmail(r, uid)
				if err == nil && email != "current@example.com" {
					return fmt.Errorf("wrong verified email: %q", email)
				}
				return err
			}
			if mode == "concurrent requests" {
				results := make(chan error, 8)
				for i := 0; i < cap(results); i++ {
					go func() { results <- verify(token) }()
				}
				for i := 0; i < cap(results); i++ {
					if err := <-results; err != nil {
						t.Errorf("concurrent verification failed: %v", err)
					}
				}
			} else {
				err := verify(token)
				wantError := mode == "identity lookup failure" || mode == "revoked during refresh"
				if (err != nil) != wantError {
					t.Fatalf("first verification: %v", err)
				}
			}
			if err := verify(sibling); err != nil {
				t.Errorf("sibling lost the rotated token: %v", err)
			}
			if refreshes != 1 {
				t.Errorf("reused a single-use refresh token: %d calls", refreshes)
			}
			st := loopbackSnapshot(t, f)
			if st.Sessions[auth.Hash(sibling)].IdentityCredential == original || st.Devices["pending-cli"].IdentityCredential != st.Sessions[auth.Hash(sibling)].IdentityCredential {
				t.Error("shared login proof was not updated together")
			}
			if mode == "revoked during refresh" && st.Sessions[auth.Hash(token)].UserID != "" {
				t.Error("refresh restored a revoked session")
			}
			if st.Sessions["unrelated-login"].IdentityCredential != unrelated || len(st.Users[uid].Connections) != 0 || len(st.GitHubAppConnections) != 0 {
				t.Error("refresh changed unrelated login or source credentials")
			}
		})
	}
}
