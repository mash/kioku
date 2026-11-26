package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

func newSignupE2E(t *testing.T, value string, logins map[string]oauthTestLogin, githubOnly bool) *e2e {
	t.Helper()
	allowlist, err := ParseSignupAllowlist(value)
	if err != nil {
		t.Fatal(err)
	}
	return newConfiguredE2E(t, func(s *Server) {
		s.Config.SignupAllowlist = allowlist
		s.Config.Providers = testOAuthProviders(t, s.Config.BaseURL, logins)
		if githubOnly {
			delete(s.Config.Providers, "google")
		}
	})
}

func (e *e2e) signupCallback(provider, code string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(e.oauthCallbackRequest("/auth/"+provider+"/start", code, ""))
}

func (e *e2e) signupSQL() *sql.DB {
	e.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(e.dir, "catalog.sqlite"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { db.Close() })
	return db
}

func signupSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var state string
	err := db.QueryRowContext(t.Context(), `SELECT json_array(
		(SELECT json_group_array(json_array(id,email,data)) FROM (SELECT * FROM users ORDER BY id)),
		(SELECT json_group_array(json_array(user_id,provider,identity_id)) FROM (SELECT * FROM user_identities ORDER BY provider,identity_id)),
		(SELECT json_group_array(json_array(id,kind,status,user_id,project_id,expires,created,data)) FROM (SELECT * FROM sessions ORDER BY id)),
		(SELECT json_group_array(json_array(id,project_id,created_by,expires,used_by,token_hash,data)) FROM (SELECT * FROM invites ORDER BY id))
	)`).Scan(&state)
	if err != nil {
		t.Fatal(err)
	}
	return auth.Hash(state)
}

func signupCookies(t *testing.T, res *httptest.ResponseRecorder, issued bool) string {
	t.Helper()
	var cleared bool
	var token string
	for _, cookie := range res.Result().Cookies() {
		switch cookie.Name {
		case "kioku_oauth":
			cleared = cookie.Path == "/auth/" && cookie.Value == "" && cookie.MaxAge < 0 && cookie.HttpOnly && cookie.SameSite == http.SameSiteLaxMode
		case "kioku_session":
			if !issued {
				t.Fatal("refusal modified the session cookie")
			}
			token = cookie.Value
		}
	}
	if !cleared || issued != (token != "") {
		t.Fatalf("wrong OAuth or session cookies: cleared=%v issued=%v", cleared, token != "")
	}
	return token
}

func assertSignupDenied(t *testing.T, res *httptest.ResponseRecorder) {
	t.Helper()
	const message = "このログイン ID では Kioku のアカウントを作れません。管理者に問い合わせてください。"
	if res.Code != http.StatusForbidden || res.Header().Get("Location") != "" ||
		res.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		!strings.Contains(res.Body.String(), message) || strings.Contains(res.Body.String(), "nature.global") {
		t.Fatalf("not a signup refusal: %d %s", res.Code, res.Body)
	}
	signupCookies(t, res, false)
}

func TestE2ESignupPolicy(t *testing.T) {
	t.Parallel()
	const restricted = " Nature.Global, , partner-2.example, kioku.example, "
	for _, tc := range []struct {
		name, allowlist string
		login           oauthTestLogin
		want            int
	}{
		{"case-insensitive", restricted, oauthTestLogin{hd: "Nature.Global"}, 303},
		{"second-domain", restricted, oauthTestLogin{hd: "Partner-2.Example"}, 303},
		{"untrusted-sources", restricted, oauthTestLogin{email: "alice@nature.global", userInfoHD: "nature.global"}, 403},
		{"non-string-hd", restricted, oauthTestLogin{hd: []string{"nature.global"}}, 403},
		{"wrong-claim-name", restricted, oauthTestLogin{tokenClaims: map[string]any{"HD": "nature.global"}}, 403},
		{"subdomain", restricted, oauthTestLogin{hd: "sub.nature.global"}, 403},
		{"not-trimmed", restricted, oauthTestLogin{hd: "nature.global "}, 403},
		{"non-ascii-hd", restricted, oauthTestLogin{hd: "Kioku.example"}, 403},
		{"empty-list", ", ,", oauthTestLogin{}, 303},
		{"invalid-token", restricted, oauthTestLogin{hd: "nature.global", tokenClaims: map[string]any{"aud": "wrong-audience"}}, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.login.subject = "alice"
			e := newSignupE2E(t, tc.allowlist, map[string]oauthTestLogin{"alice": tc.login}, false)
			db := e.signupSQL()
			before := signupSnapshot(t, db)
			res := e.signupCallback("google", "alice")
			e.expect(res, tc.want, nil)
			if tc.want == 303 {
				if res.Header().Get("Location") != "/" {
					t.Fatal("wrong default return path")
				}
				e.expect(e.call("GET", "/api/me", signupCookies(t, res, true), nil), 200, nil)
				return
			}
			if tc.want == 403 {
				assertSignupDenied(t, res)
			} else if !strings.HasPrefix(res.Header().Get("Content-Type"), "application/json") || res.Header().Get("Set-Cookie") != "" {
				t.Fatal("changed authentication-failure response")
			}
			if signupSnapshot(t, db) != before {
				t.Fatal("rejected signup changed the database")
			}
		})
	}
}

func TestE2ESignupGitHub(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"open", "restricted", "github-only"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			const email = "alice@nature.global"
			value := "nature.global"
			if mode == "open" {
				value = ""
			}
			e := newSignupE2E(t, value, map[string]oauthTestLogin{
				"alice": {subject: "alice", email: email, hd: "nature.global"},
				"123":   {subject: "123", email: email},
			}, mode == "github-only")
			db := e.signupSQL()
			switch mode {
			case "restricted":
				e.expect(e.signupCallback("google", "alice"), 303, nil)
			case "github-only":
				if err := e.db.Write(t.Context(), func(q *store.Queries) error {
					if err := q.PutUser(store.UserRow{ID: "registered", Email: email}); err != nil {
						return err
					}
					return q.PutIdentity("registered", "github", "123")
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := signupSnapshot(t, db)
			res := e.signupCallback("github", "123")
			if mode == "restricted" {
				assertSignupDenied(t, res)
				if signupSnapshot(t, db) != before {
					t.Fatal("GitHub signup changed the database")
				}
				return
			}
			e.expect(res, 303, nil)
			e.expect(e.call("GET", "/api/me", signupCookies(t, res, true), nil), 200, nil)
			if mode == "github-only" {
				assertSignupDenied(t, e.signupCallback("github", "456"))
			}
		})
	}
}

func TestE2ESignupMCPAndInvite(t *testing.T) {
	t.Parallel()
	e := newSignupE2E(t, "nature.global", map[string]oauthTestLogin{
		"allowed": {subject: "owner", hd: "nature.global"},
		"outside": {subject: "outside", hd: "other.example"},
	}, false)
	const start = "/auth/google/start"
	owner := e.signupCallback("google", "allowed")
	e.expect(owner, 303, nil)
	token := signupCookies(t, owner, true)
	project := e.project(token, "Signup test")
	mcp, _ := e.tokens(e.oauthCode(token, project))
	var invite struct{ URL string }
	e.expect(e.call("POST", "/api/projects/"+project+"/invites", token, map[string]string{"email": "invitee@example.com"}), 201, &invite)
	e.expect(e.call("GET", invite.URL, "", nil), 200, nil)
	var client struct {
		ID string `json:"client_id"`
	}
	redirectURI := e.url + "/callback"
	e.expect(e.call("POST", "/oauth/register", "", map[string]any{"redirect_uris": []string{redirectURI}}), 201, &client)
	target := "/oauth/authorize?" + url.Values{
		"client_id": {client.ID}, "redirect_uri": {redirectURI}, "response_type": {"code"},
		"code_challenge_method": {"S256"}, "code_challenge": {auth.Challenge(auth.Token())},
		"state": {auth.Token()}, "resource": {e.url + "/mcp"},
	}.Encode()
	e.expect(e.call("GET", target, "", nil), 302, nil)
	db := e.signupSQL()
	before := signupSnapshot(t, db)
	assertSignupDenied(t, e.do(e.oauthCallbackRequest(start, "invitee", token)))
	mcpStart := start + "?" + url.Values{"return": {target}}.Encode()
	assertSignupDenied(t, e.do(e.oauthCallbackRequest(mcpStart, "outside", "")))
	if signupSnapshot(t, db) != before {
		t.Fatal("refusals changed account, invite or authorization state")
	}
	e.expect(e.call("GET", target, token, nil), 200, nil)
	e.ping(mcp, 200)
	existing := e.do(e.oauthCallbackRequest(mcpStart, "owner", ""))
	e.expect(existing, 303, nil)
	if existing.Header().Get("Location") != target {
		t.Fatal("known Google ID without hd was not redirected to MCP consent")
	}
}

func TestE2ESignupConcurrent(t *testing.T) {
	t.Parallel()
	e := newSignupE2E(t, "nature.global", map[string]oauthTestLogin{
		"first": {subject: "alice", hd: "nature.global"}, "second": {subject: "alice", hd: "nature.global"},
	}, false)
	start := "/auth/google/start"
	requests := []*http.Request{e.oauthCallbackRequest(start, "first", ""), e.oauthCallbackRequest(start, "second", "")}
	var group sync.WaitGroup
	for _, req := range requests {
		group.Go(func() {
			res, err := e.client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			res.Body.Close()
			if res.StatusCode != 303 {
				t.Errorf("callback status = %d, want 303", res.StatusCode)
			}
		})
	}
	group.Wait()
	var users, ids, sessions int
	err := e.signupSQL().QueryRow("SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM user_identities), (SELECT count(*) FROM sessions)").Scan(&users, &ids, &sessions)
	if err != nil || users != 1 || ids != 1 || sessions != 2 {
		t.Fatalf("users=%d identities=%d sessions=%d: %v", users, ids, sessions, err)
	}
}

func TestE2ESignupDatabaseFailures(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{"identity-lookup", "missing-user", "write"} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			e := newSignupE2E(t, "nature.global", map[string]oauthTestLogin{
				"allowed": {subject: "member", hd: "nature.global"},
				"denied":  {subject: "member"},
			}, false)
			db := e.signupSQL()
			code := "allowed"
			before := signupSnapshot(t, db)
			switch fault {
			case "identity-lookup":
				// The identity lookup must fail, not fall through to account creation.
				if _, err := db.Exec("ALTER TABLE user_identities RENAME TO unavailable_identities"); err != nil {
					t.Fatal(err)
				}
			case "missing-user":
				// Establish the identity through OAuth, then isolate a broken reference.
				e.expect(e.signupCallback("google", "allowed"), 303, nil)
				if _, err := db.Exec("DELETE FROM users"); err != nil {
					t.Fatal(err)
				}
				code = "denied" // An orphan is a server error, not signup denied.
				before = signupSnapshot(t, db)
			case "write":
				if _, err := db.Exec(`CREATE TRIGGER fail_session BEFORE INSERT ON sessions
					BEGIN SELECT RAISE(ABORT, 'injected signup insert failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			callback := e.signupCallback("google", code)
			e.expect(callback, 500, nil)
			if callback.Header().Get("Set-Cookie") != "" || callback.Header().Get("Location") != "" ||
				!strings.Contains(callback.Body.String(), "internal server error") {
				t.Fatal("store failure advertised a successful login or a signup refusal")
			}
			if fault == "identity-lookup" {
				if _, err := db.Exec("ALTER TABLE unavailable_identities RENAME TO user_identities"); err != nil {
					t.Fatal(err)
				}
			}
			if signupSnapshot(t, db) != before {
				t.Fatal("SQLite failure left partial authentication state")
			}
		})
	}
}
