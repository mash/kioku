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
	"time"

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
	if (!issued && !cleared) || issued != (token != "") {
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
			if tc.want == 303 {
				signupCookies(t, res, false)
				res = e.submitSignup(res, "")
				e.expect(res, tc.want, nil)
				if res.Header().Get("Location") != "/" {
					t.Fatal("wrong default return path")
				}
				e.expect(e.call("GET", "/api/me", signupCookies(t, res, true), nil), 200, nil)
				return
			}
			e.expect(res, tc.want, nil)
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
				e.expect(e.submitSignup(e.signupCallback("google", "alice"), ""), 303, nil)
			case "github-only":
				if err := e.db.Write(t.Context(), func(q *store.Queries) error {
					if err := createAccount(q, "registered", "alice", email, "Alice", 1); err != nil {
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
			if mode == "open" {
				res = e.submitSignup(res, "")
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
	owner := e.submitSignup(e.signupCallback("google", "allowed"), "")
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
	var requests []*http.Request
	for _, code := range []string{"first", "second"} {
		page := e.do(e.oauthCallbackRequest(start, code, ""))
		e.expect(page, 200, nil)
		requests = append(requests, signupFormRequest(t, e.url, page.Result().Cookies(), page.Body.String(), "alice"))
	}
	var group sync.WaitGroup
	results := make(chan int, len(requests))
	for _, req := range requests {
		group.Go(func() {
			res, err := e.client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			res.Body.Close()
			results <- res.StatusCode
		})
	}
	group.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[303] != 1 || counts[409] != 1 {
		t.Fatal("signup results", counts)
	}
	var users, ids, sessions int
	err := e.signupSQL().QueryRow("SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM user_identities), (SELECT count(*) FROM sessions WHERE kind='user')").Scan(&users, &ids, &sessions)
	if err != nil || users != 1 || ids != 1 || sessions != 1 {
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
				e.expect(e.submitSignup(e.signupCallback("google", "allowed"), ""), 303, nil)
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

func TestE2EUserSlugChoiceAndReservation(t *testing.T) {
	t.Parallel()
	e := newSignupE2E(t, "", map[string]oauthTestLogin{
		"first":  {subject: "first", email: "Alice.Smith+Work@example.com"},
		"second": {subject: "second", email: "API@example.com"},
		"repeat": {subject: "first", email: "new-email@example.com"},
	}, false)
	db := e.signupSQL()
	page := e.signupCallback("google", "first")
	e.expect(page, 200, nil)
	signupCookies(t, page, false)
	if !strings.Contains(page.Body.String(), `name="slug" type="text" value="alice-smith-work"`) {
		t.Fatal("wrong default slug", page.Body)
	}
	var total int
	if err := db.QueryRow("SELECT (SELECT count(*) FROM users)+(SELECT count(*) FROM projects)+(SELECT count(*) FROM organizations)").Scan(&total); err != nil || total != 0 {
		t.Fatal("signup created records before the form was submitted", total, err)
	}

	badCSRF := signupFormRequest(t, e.url, page.Result().Cookies(), page.Body.String(), "chosen")
	badCSRF.Header.Set("Cookie", "kioku_signup=wrong")
	e.expect(e.do(badCSRF), 403, nil)
	for _, slug := range []string{"api", "u-private", "Upper", "with--gap"} {
		invalid := e.submitSignup(page, slug)
		e.expect(invalid, 400, nil)
		if !strings.Contains(invalid.Body.String(), "ユーザースラッグには") {
			t.Fatal("missing signup validation message", invalid.Body)
		}
	}
	first := e.submitSignup(page, "chosen")
	e.expect(first, 303, nil)
	token := signupCookies(t, first, true)
	e.expect(e.submitSignup(page, "cannot-replay"), 410, nil)

	var project, org, slug, namespace string
	err := db.QueryRow(`SELECT u.general_project_id,u.personal_org_id,o.slug,o.slug||'/'||p.slug
 FROM users u JOIN organizations o ON o.id=u.personal_org_id
 JOIN projects p ON p.id=u.general_project_id AND p.org_id=o.id`).Scan(&project, &org, &slug, &namespace)
	if err != nil || project == "" || org == "" || slug != "chosen" || namespace != "chosen/general" {
		t.Fatal("invalid personal org/General references", slug, namespace, err)
	}
	e.expect(e.signupCallback("google", "repeat"), 303, nil)
	if err = db.QueryRow("SELECT count(*) FROM projects WHERE org_id=?", org).Scan(&total); err != nil || total != 1 {
		t.Fatal("repeat login changed General", total, err)
	}

	second := e.signupCallback("google", "second")
	e.expect(second, 200, nil)
	if !strings.Contains(second.Body.String(), `name="slug" type="text" value="user-api"`) {
		t.Fatal("reserved email local part produced an invalid default", second.Body)
	}
	for _, name := range []string{"chosen", "team"} {
		if name == "team" {
			e.expect(e.call("POST", "/api/orgs", token, map[string]string{"slug": name}), 201, nil)
		}
		occupied := e.submitSignup(second, name)
		e.expect(occupied, 409, nil)
		if !strings.Contains(occupied.Body.String(), "使用中か予約済み") {
			t.Fatal("missing slug conflict message", occupied.Body)
		}
	}
	e.expect(e.call("DELETE", "/api/account", token, map[string]string{"confirm": "delete-account"}), 200, nil)
	e.expect(e.submitSignup(second, "chosen"), 409, nil)
	var expires int64
	if err = db.QueryRow("SELECT expires FROM user_slug_reservations WHERE slug='chosen'").Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if duration := time.Until(time.UnixMilli(expires)); duration <= 90*24*time.Hour-time.Minute || duration > 90*24*time.Hour {
		t.Fatal("slug was not reserved for 90 days", duration)
	}
	another := e.submitSignup(e.signupCallback("google", "third"), "third")
	e.expect(another, 303, nil)
	otherToken := signupCookies(t, another, true)
	e.expect(e.call("POST", "/api/orgs", otherToken, map[string]string{"slug": "chosen"}), 409, nil)
	if _, err = db.Exec("UPDATE user_slug_reservations SET expires=0 WHERE slug='chosen'"); err != nil {
		t.Fatal(err)
	}
	registered := e.submitSignup(second, "chosen")
	e.expect(registered, 303, nil)
	e.expect(e.call("DELETE", "/api/account", signupCookies(t, registered, true), map[string]string{"confirm": "delete-account"}), 200, nil)
	if _, err = db.Exec("UPDATE user_slug_reservations SET expires=0 WHERE slug='chosen'"); err != nil {
		t.Fatal(err)
	}
	e.expect(e.call("POST", "/api/orgs", otherToken, map[string]string{"slug": "chosen"}), 201, nil)
}
