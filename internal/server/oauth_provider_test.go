package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"golang.org/x/net/html"
)

func connectorClient(t *testing.T, f *fixture) string {
	t.Helper()
	var client map[string]any
	if status := f.request("POST", "/oauth/register", "", map[string]any{"client_name": "Test connector", "redirect_uris": []string{f.url + "/callback"}}, &client); status != 201 {
		t.Fatal(status, client)
	}
	return client["client_id"].(string)
}
func connectorForm(f *fixture, method, path, human string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if method == "GET" {
		path += "?" + form.Encode()
	} else {
		body = form.Encode()
	}
	req := httptest.NewRequest(method, f.url+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if human != "" {
		req.AddCookie(&http.Cookie{Name: "kioku_session", Value: human})
		req.Header.Set("Origin", f.url)
	}
	out := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(out, req)
	return out
}
func connectorAuthorizeParams(f *fixture, client, verifier string) url.Values {
	return url.Values{"client_id": {client}, "redirect_uri": {f.url + "/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {auth.Challenge(verifier)}, "state": {"opaque-client-state"}, "resource": {f.url + "/mcp"}}
}

// Read the actual form as the browser would, including its decoded action URL.
func consentControls(t *testing.T, page *httptest.ResponseRecorder) (action string, fields url.Values, projectIDs []string) {
	t.Helper()
	fields = url.Values{}
	doc, err := html.Parse(page.Body)
	if err != nil {
		t.Fatal(err)
	}
	for n := range doc.Descendants() {
		if n.Type != html.ElementNode || (n.Data != "form" && n.Data != "input") {
			continue
		}
		attrs := map[string]string{}
		for _, a := range n.Attr {
			attrs[a.Key] = a.Val
		}
		if n.Data == "form" {
			action = attrs["action"]
		} else if attrs["name"] == "project_id" {
			if _, checked := attrs["checked"]; checked || attrs["type"] != "checkbox" {
				t.Fatal("project selection must be an unchecked checkbox")
			}
			projectIDs = append(projectIDs, attrs["value"])
		} else if attrs["type"] == "hidden" {
			fields.Add(attrs["name"], attrs["value"])
		}
	}
	return action, fields, projectIDs
}

func connectorAuthCode(t *testing.T, f *fixture, client, human string, projectIDs ...string) (string, url.Values) {
	t.Helper()
	verifier := auth.Token()
	form := connectorAuthorizeParams(f, client, verifier)
	page := connectorForm(f, "GET", "/oauth/authorize", human, form)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Test connector") {
		t.Fatal(page.Code, page.Body.String())
	}
	action, fields, options := consentControls(t, page)
	for _, id := range projectIDs {
		if !slices.Contains(options, id) {
			t.Fatal("selected project was not offered")
		}
	}
	fields["project_id"] = projectIDs
	result := connectorForm(f, "POST", action, human, fields)
	if result.Code != 302 {
		t.Fatal(result.Code, result.Body.String())
	}
	target, err := url.Parse(result.Header().Get("Location"))
	if err != nil || target.Query().Get("state") != form.Get("state") {
		t.Fatal("missing state", err)
	}
	code := target.Query().Get("code")
	if code == "" {
		t.Fatal("missing code")
	}
	return code, url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "redirect_uri": {f.url + "/callback"}, "code": {code}, "code_verifier": {verifier}}
}
func connectorAccessToken(t *testing.T, f *fixture, human, projectID string) string {
	t.Helper()
	_, form := connectorAuthCode(t, f, connectorClient(t, f), human, projectID)
	access, _ := connectorTokenPair(t, f, form)
	return access
}
func connectorTokenPair(t *testing.T, f *fixture, form url.Values) (string, string) {
	t.Helper()
	result := connectorForm(f, "POST", "/oauth/token", "", form)
	var tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &tokens) != nil {
		t.Fatal(result.Code, result.Body.String())
	}
	if !strings.HasPrefix(tokens.Access, "kioku_oauth.") || tokens.Refresh == "" {
		t.Fatal("missing OAuth tokens")
	}
	return tokens.Access, tokens.Refresh
}
func TestConnectorOAuthPKCEAndSingleUse(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	client := connectorClient(t, f)
	_, wrong := connectorAuthCode(t, f, client, human, project.ID)
	wrong.Set("code_verifier", strings.Repeat("x", 43))
	result := connectorForm(f, "POST", "/oauth/token", "", wrong)
	if result.Code != 400 || !strings.Contains(result.Body.String(), `"error":"invalid_grant"`) {
		t.Fatal(result.Code, result.Body.String())
	}
	_, form := connectorAuthCode(t, f, client, human, project.ID)
	if result = connectorForm(f, "POST", "/oauth/token", "", form); result.Code != 200 {
		t.Fatal(result.Code, result.Body.String())
	}
	if result = connectorForm(f, "POST", "/oauth/token", "", form); result.Code != 400 {
		t.Fatal("authorization code reused", result.Code)
	}
	token := connectorAccessToken(t, f, human, project.ID)
	if code := f.request("GET", "/api/me", token, nil, nil); code != 401 {
		t.Fatal("OAuth token authenticated API", code)
	}
}
func TestConnectorRejectsUnregisteredRedirect(t *testing.T) {
	f := setupWithWorker(t, false)
	client := connectorClient(t, f)
	form := url.Values{"client_id": {client}, "redirect_uri": {"https://attacker.example/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {auth.Challenge(auth.Token())}}
	result := connectorForm(f, "GET", "/oauth/authorize", "", form)
	if result.Code != 400 || result.Header().Get("Location") != "" {
		t.Fatal("untrusted redirect", result.Code, result.Header())
	}
}

func TestConnectorBrowserConsentPolicy(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	const callback = "https://client.example:8443/oauth/callback?provider=kioku"
	var client struct {
		ID string `json:"client_id"`
	}
	if status := f.request("POST", "/oauth/register", "", map[string]any{"client_name": "Claude", "redirect_uris": []string{callback}}, &client); status != 201 {
		t.Fatal("client registration", status)
	}
	form := url.Values{"client_id": {client.ID}, "redirect_uri": {callback}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {auth.Challenge(auth.Token())}}
	page := connectorForm(f, "GET", "/oauth/authorize", human, form)
	if page.Code != 200 || page.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatal("consent page must preserve the Origin of its own form POST", page.Code, page.Header())
	}
	if !strings.Contains(page.Body.String(), ">Claude<") || !strings.Contains(page.Body.String(), ">https://client.example:8443<") {
		t.Fatal("consent must show the registered callback origin alongside the self-reported client name")
	}
	_, sources, found := strings.Cut(page.Header().Get("Content-Security-Policy"), "form-action ")
	sources, _, _ = strings.Cut(sources, ";")
	if !found || sources != "'self' https://client.example:8443" {
		t.Fatalf("unexpected form-action sources: %q", sources)
	}
	action, fields, options := consentControls(t, page)
	if !slices.Contains(options, project.ID) {
		t.Fatal("project missing from consent form")
	}
	fields.Set("project_id", project.ID)
	if valid := connectorForm(f, "POST", action, human, fields); valid.Code != 302 {
		t.Fatal("same-origin control submission failed", valid.Code)
	}
	for _, origin := range []string{"null", "https://client.example:8443"} {
		req := httptest.NewRequest("POST", f.url+action, strings.NewReader(fields.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		req.AddCookie(&http.Cookie{Name: "kioku_session", Value: human})
		out := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(out, req)
		if out.Code != 403 || out.Header().Get("Location") != "" {
			t.Fatalf("untrusted consent origin %q: %d", origin, out.Code)
		}
	}
}

func TestConnectorRejectsNonliteralCallbackHosts(t *testing.T) {
	f := setupWithWorker(t, false)
	for _, uri := range []string{"https://*.example.com/cb", "https://one.example;script-src/cb", "https://one.example,two.example/cb"} {
		if status := f.request("POST", "/oauth/register", "", map[string]any{"redirect_uris": []string{uri}}, nil); status != 400 {
			t.Fatalf("registered unsafe callback host: %s (HTTP %d)", uri, status)
		}
	}
}

func TestConnectorRegistrationIgnoresAdditionalMetadata(t *testing.T) {
	f := setupWithWorker(t, false)
	var client map[string]any
	metadata := map[string]any{
		"client_name": "Test connector", "redirect_uris": []string{f.url + "/callback"},
		"client_uri": "https://client.example", "logo_uri": "https://client.example/logo.png",
		"example_extension": map[string]string{"version": "1"},
	}
	if status := f.request("POST", "/oauth/register", "", metadata, &client); status != 201 {
		t.Fatal("additional client metadata rejected", status, client)
	}
	id, ok := client["client_id"].(string)
	if !ok || id == "" || client["client_name"] != metadata["client_name"] || client["token_endpoint_auth_method"] != "none" || client["example_extension"] != nil {
		t.Fatal("unexpected client registration", client)
	}
}

func TestConnectorRefreshRetriesAfterCapacityFailure(t *testing.T) {
	f := setupWithWorker(t, false)
	client := connectorClient(t, f)
	human := f.login("owner@example.com")
	_, form := connectorAuthCode(t, f, client, human, securityProject(t, f, human).ID)
	response := connectorForm(f, "POST", "/oauth/token", "", form)
	var tokens struct {
		Refresh string `json:"refresh_token"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &tokens) != nil || tokens.Refresh == "" {
		t.Fatal("initial exchange failed")
	}
	if err := f.db.Update(func(st *model.State) error {
		grant := st.OAuthTokens[auth.Hash(tokens.Refresh)]
		grant.Kind, grant.Expires = "access", time.Now().Add(time.Hour)
		for i := 0; len(st.OAuthTokens) < 20000; i++ {
			st.OAuthTokens[auth.Hash("capacity-test-"+strconv.Itoa(i))] = grant
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	form = url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {tokens.Refresh}}
	response = connectorForm(f, "POST", "/oauth/token", "", form)
	if response.Code != 400 || !strings.Contains(response.Body.String(), "temporarily_unavailable") {
		t.Fatal("expected capacity failure", response.Code)
	}
	if err := f.db.Update(func(st *model.State) error { delete(st.OAuthTokens, auth.Hash("capacity-test-0")); return nil }); err != nil {
		t.Fatal(err)
	}
	if response = connectorForm(f, "POST", "/oauth/token", "", form); response.Code != 200 {
		t.Fatal("retry lost refresh credential", response.Code)
	}
}
