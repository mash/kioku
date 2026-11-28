package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/authtest"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/store"
)

// This harness creates state only through kiokud's public HTTP and MCP routes.
// Each test owns a real catalog and local objects; only external providers are fake.
type e2e struct {
	t      *testing.T
	url    string
	client *http.Client
	db     *store.Database
	dir    string
}

func newE2E(t *testing.T, configure ...func(*Server, *runners.Config)) *e2e {
	t.Helper()
	return newConfiguredE2E(t, nil, configure...)
}

func newConfiguredE2E(t *testing.T, configureServer func(*Server), configure ...func(*Server, *runners.Config)) *e2e {
	t.Helper()
	dir := t.TempDir()
	objects := local.Store{Dir: filepath.Join(dir, "objects")}
	db, cipher, err := store.OpenStorage(t.Context(), dir, objects, func(bool) (*auth.Cipher, error) {
		return auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(Config{Prompts: defaultPrompts(t), GenerateDebounce: time.Hour}, db, objects, cipher)
	ts := httptest.NewUnstartedServer(s.Handler())
	t.Cleanup(ts.Close)
	s.Config.BaseURL = "http://" + ts.Listener.Addr().String()
	if configureServer != nil {
		configureServer(s)
	}
	if s.Config.Providers == nil {
		s.Config.Providers = map[string]auth.Provider{"google": testOIDCProvider(t, s.Config.BaseURL)}
	}
	config := runners.Config{Prompts: s.Config.Prompts, BaseURL: s.Config.BaseURL, DataDir: dir}
	for _, setup := range configure {
		setup(s, &config)
	}
	ctx, cancel := context.WithCancel(t.Context())
	rs, err := runners.Load(config, func(job string) { s.ReconcileJob(ctx, job) })
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s.SetRunner(rs.Runner)
	done := make(chan error, 1)
	if len(configure) > 0 {
		go func() { done <- s.RunWorker(ctx) }()
	} else {
		done <- nil
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		if err := rs.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	ts.Start()
	client := ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &e2e{t: t, url: ts.URL, client: client, db: db, dir: dir}
}

func testOIDCProvider(t *testing.T, baseURL string) auth.Provider {
	return testOAuthProviders(t, baseURL, nil)["google"]
}

type oauthTestLogin struct {
	subject, email string
	hd, userInfoHD any
	tokenClaims    map[string]any
}

func testOAuthProviders(t *testing.T, baseURL string, logins map[string]oauthTestLogin) map[string]auth.Provider {
	google := authtest.NewGoogle(t)
	lookup := func(code string) oauthTestLogin {
		login, ok := logins[code]
		if !ok {
			login.subject = code
		}
		if login.email == "" {
			login.email = login.subject + "@example.com"
		}
		return login
	}
	client := &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		var payload any
		switch r.URL.Host {
		case "oauth2.googleapis.com", "github.com":
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			code := r.Form.Get("code")
			if code == "exchange-error" {
				return nil, errors.New("exchange failed")
			}
			reply := map[string]any{"access_token": code, "expires_in": 3600}
			if r.URL.Host == "oauth2.googleapis.com" {
				login := lookup(code)
				claims := google.Claims(login.subject, r.Form.Get("code_verifier"))
				if login.hd != nil {
					claims["hd"] = login.hd
				}
				for key, value := range login.tokenClaims {
					claims[key] = value
				}
				reply["id_token"] = google.Sign(t, claims)
			}
			payload = reply
		case "www.googleapis.com":
			payload = google.JWKS()
		case "openidconnect.googleapis.com", "api.github.com":
			code := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if code == "identity-error" {
				return nil, errors.New("identity failed")
			}
			login := lookup(code)
			switch {
			case r.URL.Host == "openidconnect.googleapis.com":
				payload = map[string]any{"sub": login.subject, "name": login.subject, "email": login.email, "email_verified": true, "hd": login.userInfoHD}
			case r.URL.Path == "/user":
				payload = map[string]any{"id": json.Number(login.subject), "login": "github-user"}
			case r.URL.Path == "/user/emails":
				payload = []map[string]any{{"email": login.email, "primary": true, "verified": true}}
			default:
				return nil, fmt.Errorf("unexpected provider path %s", r.URL.Path)
			}
		default:
			return nil, fmt.Errorf("unexpected host %s", r.URL.Host)
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b))}, nil
	})}
	return map[string]auth.Provider{
		"google": {Name: "google", ClientID: "id", ClientSecret: "secret", RedirectURL: baseURL + "/auth/google/callback", Client: client},
		"github": {Name: "github", ClientID: "id", ClientSecret: "secret", RedirectURL: baseURL + "/auth/github/callback", Client: client},
	}
}

func (e *e2e) call(method, path, token string, input any) *httptest.ResponseRecorder {
	e.t.Helper()
	var body []byte
	contentType := "application/json"
	if form, ok := input.(url.Values); ok {
		body, contentType = []byte(form.Encode()), "application/x-www-form-urlencoded"
	} else if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			e.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, e.url+strings.TrimPrefix(path, e.url), bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Kioku-Client-Version", "dev")
	if strings.HasPrefix(req.URL.Path, "/oauth/authorize") {
		req.AddCookie(&http.Cookie{Name: "kioku_session", Value: token})
		req.Header.Set("Origin", e.url)
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return e.do(req)
}
func (e *e2e) do(req *http.Request) *httptest.ResponseRecorder {
	e.t.Helper()
	res, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := httptest.NewRecorder()
	for key, values := range res.Header {
		out.Header()[key] = values
	}
	out.WriteHeader(res.StatusCode)
	if _, err := io.Copy(out, res.Body); err != nil {
		e.t.Fatal(err)
	}
	return out
}
func (e *e2e) expect(res *httptest.ResponseRecorder, status int, out any) {
	e.t.Helper()
	if res.Code != status {
		e.t.Fatalf("HTTP %d, want %d: %s", res.Code, status, res.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(res.Body.Bytes(), out); err != nil {
			e.t.Fatal(err)
		}
	}
}
func (e *e2e) oauthCallbackRequest(startURL, code, session string) *http.Request {
	e.t.Helper()
	start := e.call("GET", startURL, "", nil)
	e.expect(start, http.StatusFound, nil)
	target, err := start.Result().Location()
	if err != nil {
		e.t.Fatal(err)
	}
	callback := target.Query().Get("redirect_uri")
	if !strings.HasPrefix(callback, e.url+"/auth/") {
		e.t.Fatalf("unexpected OAuth callback %q", callback)
	}
	params := url.Values{"state": {target.Query().Get("state")}, "code": {code}}
	req, err := http.NewRequestWithContext(e.t.Context(), "GET", callback+"?"+params.Encode(), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, cookie := range start.Result().Cookies() {
		req.AddCookie(cookie)
	}
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "kioku_session", Value: session})
	}
	return req
}

func signupFormRequest(t *testing.T, base string, cookies []*http.Cookie, page, slug string) *http.Request {
	t.Helper()
	var pending *http.Cookie
	for _, cookie := range cookies {
		if cookie.Name == "kioku_signup" {
			pending = cookie
		}
	}
	if pending == nil || !strings.Contains(page, `name="csrf" value="`+auth.Hash(pending.Value)+`"`) {
		t.Fatal("signup page is missing its form token")
	}
	if slug == "" {
		_, tail, ok := strings.Cut(page, `name="slug" type="text" value="`)
		if !ok {
			t.Fatal("signup page is missing its default slug")
		}
		slug, _, _ = strings.Cut(tail, `"`)
	}
	form := url.Values{"slug": {slug}, "csrf": {auth.Hash(pending.Value)}}
	req, err := http.NewRequestWithContext(t.Context(), "POST", base+"/auth/signup", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", base)
	req.AddCookie(pending)
	return req
}

func (e *e2e) submitSignup(page *httptest.ResponseRecorder, slug string) *httptest.ResponseRecorder {
	e.t.Helper()
	e.expect(page, http.StatusOK, nil)
	return e.do(signupFormRequest(e.t, e.url, page.Result().Cookies(), page.Body.String(), slug))
}

func (e *e2e) startLoopback() loopbackAttempt {
	e.t.Helper()
	a := loopbackAttempt{verifier: auth.Token(), state: auth.Token(), redirect: "http://127.0.0.1:5000/callback"}
	var start map[string]string
	e.expect(e.call("POST", "/api/auth/loopback", "", map[string]string{
		"provider": "google", "code_challenge_method": "S256", "code_challenge": auth.Challenge(a.verifier),
		"redirect_uri": a.redirect, "state": a.state,
	}), 201, &start)
	a.device, a.startURL = start["device"], start["url"]
	return a
}
func (e *e2e) loginCode(subject string) map[string]string {
	e.t.Helper()
	a := e.startLoopback()
	callback := e.do(e.oauthCallbackRequest(a.startURL, subject, ""))
	if callback.Code == http.StatusOK {
		callback = e.submitSignup(callback, "")
	}
	e.expect(callback, 302, nil)
	target, err := callback.Result().Location()
	if err != nil || target.Query().Get("state") != a.state || target.Query().Get("code") == "" {
		e.t.Fatal("invalid callback", err)
	}
	return map[string]string{"device": a.device, "code": target.Query().Get("code"), "code_verifier": a.verifier}
}
func (e *e2e) login(subject string) string {
	e.t.Helper()
	var result map[string]string
	e.expect(e.call("POST", "/api/auth/loopback/token", "", e.loginCode(subject)), 200, &result)
	return result["token"]
}
func (e *e2e) project(token, name string) string {
	e.t.Helper()
	var p model.Project
	e.expect(e.call("POST", "/api/projects", token, map[string]string{"name": name}), 201, &p)
	return p.Namespace
}
func (e *e2e) oauthCode(human, namespace string) url.Values {
	e.t.Helper()
	var client struct {
		ID string `json:"client_id"`
	}
	e.expect(e.call("POST", "/oauth/register", "", map[string]any{"client_name": "Test connector", "redirect_uris": []string{e.url + "/callback"}}), 201, &client)
	verifier := auth.Token()
	params := url.Values{"client_id": {client.ID}, "redirect_uri": {e.url + "/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {auth.Challenge(verifier)}, "state": {auth.Token()}, "resource": {e.url + "/mcp"}}
	page := e.call("GET", "/oauth/authorize?"+params.Encode(), human, nil)
	e.expect(page, 200, nil)
	action, fields, offered := consentControls(e.t, page)
	if !slices.Contains(offered, namespace) {
		e.t.Fatal("missing project in consent")
	}
	fields["namespace"] = []string{namespace}
	approved := e.call("POST", action, human, fields)
	e.expect(approved, 303, nil)
	target, err := url.Parse(approved.Header().Get("Location"))
	if err != nil || target.Query().Get("state") != params.Get("state") {
		e.t.Fatal("invalid consent redirect", err)
	}
	return url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ID}, "redirect_uri": {e.url + "/callback"}, "code": {target.Query().Get("code")}, "code_verifier": {verifier}}
}
func (e *e2e) tokens(form url.Values) (string, string) {
	e.t.Helper()
	var result map[string]any
	e.expect(e.call("POST", "/oauth/token", "", form), 200, &result)
	return result["access_token"].(string), result["refresh_token"].(string)
}
func (e *e2e) ping(token string, status int) {
	e.t.Helper()
	e.expect(e.call("POST", "/mcp", token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}), status, nil)
}
func (e *e2e) tool(token, name string, args any, failed bool, out any) {
	e.t.Helper()
	var result struct {
		Result struct {
			IsError bool
			Content []struct{ Text string }
		}
	}
	e.expect(e.call("POST", "/mcp", token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}), 200, &result)
	if result.Result.IsError != failed || len(result.Result.Content) != 1 {
		e.t.Fatal("unexpected MCP result", result)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(result.Result.Content[0].Text), out); err != nil {
			e.t.Fatal(err)
		}
	}
}

func TestE2ELoopbackPKCEAndSingleUse(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	for _, field := range []string{"code", "code_verifier"} {
		exchange := e.loginCode("owner")
		correct := exchange[field]
		exchange[field] = auth.Token()
		e.expect(e.call("POST", "/api/auth/loopback/token", "", exchange), 410, nil)
		exchange[field] = correct
		e.expect(e.call("POST", "/api/auth/loopback/token", "", exchange), 410, nil)
	}
	exchange := e.loginCode("owner")
	var issued map[string]string
	e.expect(e.call("POST", "/api/auth/loopback/token", "", exchange), 200, &issued)
	e.expect(e.call("POST", "/api/auth/loopback/token", "", exchange), 410, nil)
	e.expect(e.call("GET", "/api/account", issued["token"], nil), 200, nil)
}
func TestE2EOAuthConcurrentExchangeAndReplay(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"authorization_code", "refresh_token"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			e := newE2E(t)
			human := e.login("owner")
			namespace := e.project(human, "shared")
			form := e.oauthCode(human, namespace)
			independent, _ := e.tokens(e.oauthCode(human, namespace))
			verifier := form.Get("code_verifier")
			form.Set("code_verifier", auth.Token())
			e.expect(e.call("POST", "/oauth/token", "", form), 400, nil)
			form.Set("code_verifier", verifier)
			if kind == "refresh_token" {
				_, refresh := e.tokens(form)
				form = refreshForm(form.Get("client_id"), refresh)
			}
			results := make(chan *httptest.ResponseRecorder, 2)
			var wg sync.WaitGroup
			for range 2 {
				wg.Go(func() { results <- e.call("POST", "/oauth/token", "", form) })
			}
			wg.Wait()
			close(results)
			success, rejected := 0, 0
			for res := range results {
				if res.Code == 400 {
					rejected++
					continue
				}
				var issued map[string]any
				e.expect(res, 200, &issued)
				success++
				e.ping(issued["access_token"].(string), 401)
				e.expect(e.call("POST", "/oauth/token", "", refreshForm(form.Get("client_id"), issued["refresh_token"].(string))), 400, nil)
			}
			if success != 1 || rejected != 1 {
				t.Fatalf("success=%d rejection=%d", success, rejected)
			}
			e.ping(independent, 200)
		})
	}
}
func TestE2EAgentScopeSingleUseAndRevocation(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	human := e.login("owner")
	project, other := e.project(human, "selected"), e.project(human, "unselected")
	access, _ := e.tokens(e.oauthCode(human, project))
	e.tool(access, "cli_login_code", map[string]string{"project": other}, true, nil)
	var grant struct{ Code string }
	e.tool(access, "cli_login_code", map[string]string{"project": project, "role": "read"}, false, &grant)
	e.expect(e.call("POST", "/api/agent/login", "", map[string]string{"code": grant.Code, "name": " "}), 400, nil)
	e.expect(e.call("POST", "/api/agent/login", "", map[string]string{"code": grant.Code, "name": "reader"}), 401, nil)
	e.tool(access, "cli_login_code", map[string]string{"project": project, "role": "read"}, false, &grant)
	var issued struct{ Token string }
	e.expect(e.call("POST", "/api/agent/login", "", map[string]string{"code": grant.Code, "name": "reader"}), 200, &issued)
	e.expect(e.call("POST", "/api/projects/"+project+"/agents", human, map[string]string{"name": "reader", "role": "read"}), 405, nil)
	var projects []model.Project
	e.expect(e.call("GET", "/api/projects", issued.Token, nil), 200, &projects)
	if len(projects) != 1 || projects[0].Namespace != project {
		t.Fatal("agent enumerated another project", projects)
	}
	for _, changed := range []string{issued.Token + "bad", strings.Replace(issued.Token, "kioku_agent.", "kioku_agent.other", 1)} {
		e.expect(e.call("GET", "/api/me", changed, nil), 401, nil)
	}
	for _, suffix := range []string{"/snapshot?manifest=1", "/usage", "/jobs/unknown"} {
		e.expect(e.call("GET", "/api/projects/"+other+suffix, issued.Token, nil), 404, nil)
	}
	for _, route := range []struct{ method, path string }{
		{"POST", "/api/projects"}, {"GET", "/api/orgs"}, {"POST", "/api/account/revoke-all"},
		{"POST", "/api/projects/" + project + "/invites"}, {"DELETE", "/api/projects/" + project},
		{"POST", "/api/projects/" + project + "/put"},
		{"GET", "/api/projects/" + project + "/jobs/unknown/agent/input"},
	} {
		e.expect(e.call(route.method, route.path, issued.Token, nil), 403, nil)
	}
	listed := e.call("GET", "/api/projects/"+project+"/agents", human, nil)
	e.expect(listed, 200, nil)
	if strings.Contains(listed.Body.String(), issued.Token) || strings.Contains(listed.Body.String(), "token_hash") {
		t.Fatal("agent list exposed credentials")
	}
	e.tool(access, "view_list", map[string]string{"project": project}, false, nil)
	e.tool(access, "view_list", map[string]string{"project": other}, true, nil)
	e.expect(e.call("GET", "/api/projects/"+project+"/snapshot?manifest=1", issued.Token, nil), 200, nil)
	e.expect(e.call("GET", "/api/projects/"+other+"/snapshot?manifest=1", issued.Token, nil), 404, nil)
	e.expect(e.call("POST", "/api/projects/"+project+"/generate", issued.Token, nil), 403, nil)
	e.expect(e.call("GET", "/api/account", issued.Token, nil), 403, nil)
	e.expect(e.call("POST", "/api/logout", issued.Token, nil), 200, nil)
	e.expect(e.call("GET", "/api/projects", issued.Token, nil), 401, nil)
	e.expect(e.call("GET", "/api/me", human, nil), 200, nil)
	e.tool(access, "cli_login_code", map[string]string{"project": project, "role": "write"}, false, &grant)
	e.expect(e.call("POST", "/api/agent/login", "", map[string]string{"code": grant.Code, "name": "writer"}), 200, &issued)
	e.expect(e.call("GET", "/api/projects/"+project+"/jobs/unknown/agent/input", issued.Token, nil), 403, nil)
	var agents []map[string]any
	e.expect(e.call("GET", "/api/projects/"+project+"/agents", human, nil), 200, &agents)
	if len(agents) != 1 || agents[0]["name"] != "connector:writer" {
		t.Fatal("agent list kept ended credentials", agents)
	}
	revoke := "/api/projects/" + project + "/agents/" + agents[0]["id"].(string)
	e.expect(e.call("DELETE", revoke, human, nil), 200, nil)
	e.expect(e.call("GET", "/api/me", issued.Token, nil), 401, nil)
	e.expect(e.call("DELETE", revoke, human, nil), 404, nil)
	e.expect(e.call("GET", "/api/projects/"+project+"/agents", human, nil), 200, &agents)
	if len(agents) != 0 {
		t.Fatal("revoked agent still listed", agents)
	}
	e.tool(access, "cli_login_code", map[string]string{"project": project, "role": "write"}, false, &grant)
	e.expect(e.call("POST", "/api/agent/login", "", map[string]string{"code": grant.Code, "name": "writer"}), 200, &issued)
	e.expect(e.call("POST", "/api/account/revoke-all", human, nil), 200, nil)
	e.expect(e.call("GET", "/api/me", issued.Token, nil), 401, nil)
	e.expect(e.call("GET", "/api/me", human, nil), 401, nil)
	e.ping(access, 401)
}

func TestE2EOAuthRotationAndRejectedExchanges(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	human := e.login("owner")
	ns := e.project(human, "rotation")
	form := e.oauthCode(human, ns)
	independentForm := e.oauthCode(human, ns)
	for _, change := range []struct{ key, value string }{
		{"code_verifier", auth.Token()}, {"redirect_uri", e.url + "/wrong"},
		{"client_id", independentForm.Get("client_id")}, {"resource", "https://wrong.example/mcp"},
	} {
		bad := url.Values{}
		for k, v := range form {
			bad[k] = slices.Clone(v)
		}
		bad.Set(change.key, change.value)
		res := e.call("POST", "/oauth/token", "", bad)
		if res.Code != 400 && res.Code != 401 {
			t.Fatalf("%s: %d %s", change.key, res.Code, res.Body.String())
		}
	}
	independent, _ := e.tokens(independentForm)
	access, refresh := e.tokens(form)
	access2, refresh2 := e.tokens(refreshForm(form.Get("client_id"), refresh))
	e.ping(access, 401)
	access3, refresh3 := e.tokens(refreshForm(form.Get("client_id"), refresh2))
	e.ping(access2, 401)
	e.ping(access3, 200)
	e.expect(e.call("POST", "/oauth/token", "", refreshForm(form.Get("client_id"), refresh)), 400, nil)
	e.ping(access3, 401)
	e.expect(e.call("POST", "/oauth/token", "", refreshForm(form.Get("client_id"), refresh3)), 400, nil)
	e.ping(independent, 200)
}

func TestE2EAccountSessionRevocation(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	human, second, other := e.login("owner"), e.login("owner"), e.login("other")
	res := e.call("GET", "/api/account", human, nil)
	e.expect(res, 200, nil)
	for _, secret := range []string{human, second, "identity_access_token", "identity_refresh_token"} {
		if strings.Contains(res.Body.String(), secret) {
			t.Fatal("account exposed credential")
		}
	}
	e.expect(e.call("DELETE", "/api/account/sessions/"+auth.Hash(other), human, nil), 404, nil)
	e.expect(e.call("DELETE", "/api/account/sessions/"+auth.Hash(second), human, nil), 200, nil)
	e.expect(e.call("GET", "/api/me", second, nil), 401, nil)
	e.expect(e.call("POST", "/api/account/revoke-all", human, nil), 200, nil)
	e.expect(e.call("GET", "/api/me", human, nil), 401, nil)
	e.expect(e.call("GET", "/api/me", other, nil), 200, nil)
}

func TestE2EAgentLoginRejectsExpiredAccessToken(t *testing.T) {
	t.Parallel()
	e := newE2E(t)
	human := e.login("owner")
	project := e.project(human, "selected")
	access, _ := e.tokens(e.oauthCode(human, project))
	var grant struct{ Code string }
	e.tool(access, "cli_login_code", map[string]string{"project": project, "role": "read"}, false, &grant)
	// The access token expires after the login code was issued and before the agent exchanges it.
	if err := e.db.Write(t.Context(), func(q *store.Queries) error {
		owner, err := q.Session(auth.Hash(human))
		if err != nil {
			return err
		}
		rows, err := q.UserSessions(owner.UserID, "mcp")
		if err != nil {
			return err
		}
		for _, row := range rows {
			row.Expires = time.Now().Add(-time.Second).UnixMilli()
			if err := q.PutSession(row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.expect(e.call("POST", "/api/agent/login", "", map[string]string{"code": grant.Code, "name": "reader"}), 404, nil)
}
