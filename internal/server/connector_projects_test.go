package server

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func consentProject(t *testing.T, f *fixture, human, name string) model.Project {
	t.Helper()
	var p model.Project
	if status := f.request("POST", "/api/projects", human, map[string]string{"name": name}, &p); status != 201 {
		t.Fatal("create project", status)
	}
	return p
}

func joinConsentProject(t *testing.T, f *fixture, projectID, owner, newMember string) {
	t.Helper()
	var invite struct{ URL string }
	if status := f.request("POST", "/api/projects/"+projectID+"/invites", owner, map[string]string{}, &invite); status != 201 {
		t.Fatal("invite", status)
	}
	if status := f.request("POST", "/api/join", newMember, map[string]string{"token": invite.URL}, nil); status != 200 {
		t.Fatal("join", status)
	}
}

func expectProjectDenied(t *testing.T, f *fixture, access, namespace string) {
	t.Helper()
	raw := connectorRPC(t, f, access, "tools/call", map[string]any{"name": "cli_login_code", "arguments": map[string]string{"project": namespace}})
	var result struct {
		IsError bool
		Content []struct{ Text string }
	}
	if json.Unmarshal(raw, &result) != nil || !result.IsError || len(result.Content) != 1 || result.Content[0].Text != "project not found" {
		t.Fatal("expected indistinguishable project-not-found response", namespace)
	}
}

func TestConnectorConsentProjectSelection(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("member@example.com")
	params := connectorAuthorizeParams(f, connectorClient(t, f), auth.Token())
	if page := connectorForm(f, "GET", "/oauth/authorize", human, params); page.Code != 200 || !strings.Contains(page.Body.String(), "Create or join") || strings.Contains(page.Body.String(), "<form") {
		t.Fatal("expected instructions for a user without projects")
	}
	a := consentProject(t, f, human, "alpha")
	b := consentProject(t, f, human, "beta")
	foreign := consentProject(t, f, f.login("outsider@example.com"), "private")
	params.Set("namespace", b.Namespace) // An untrusted connector supplies the authorization URL.
	page := connectorForm(f, "GET", "/oauth/authorize", human, params)
	if page.Code != 200 || !strings.Contains(page.Body.String(), a.Namespace) || !strings.Contains(page.Body.String(), b.Namespace) || strings.Contains(page.Body.String(), foreign.Namespace) {
		t.Fatal("consent must list only the user's project namespaces")
	}
	action, fields, options := consentControls(t, page)
	if !slices.Equal(options, []string{a.Namespace, b.Namespace}) || strings.Contains(action, "namespace") {
		t.Fatal("expected exactly two unchecked projects in namespace order")
	}
	if res := connectorForm(f, "POST", action+"&namespace="+b.Namespace, human, fields); res.Code != 400 || res.Header().Get("Location") != "" || !strings.Contains(res.Body.String(), "No project was selected") {
		t.Fatal("query-only consent granted access", res.Code)
	}
	fields["namespace"] = []string{a.Namespace, foreign.Namespace}
	if res := connectorForm(f, "POST", action, human, fields); res.Code != 400 || res.Header().Get("Location") != "" {
		t.Fatal("forged non-member selection granted access", res.Code)
	}
}

func TestConnectorRefreshPreservesSelectedProjects(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	a := consentProject(t, f, human, "alpha")
	b := consentProject(t, f, human, "beta")
	unselected := consentProject(t, f, human, "not-selected")
	client := connectorClient(t, f)
	_, form := connectorAuthCode(t, f, client, human, a.ID, b.ID)
	newProject := consentProject(t, f, human, "joined-later")
	for range 3 { // Check the original grant and two rotations, including the replacement refresh token.
		access, refresh := connectorTokenPair(t, f, form)
		connectorCode(t, f, access, a.Namespace)
		connectorCode(t, f, access, b.Namespace)
		expectProjectDenied(t, f, access, unselected.Namespace)
		expectProjectDenied(t, f, access, newProject.Namespace)
		form = url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {refresh}, "namespace": {unselected.ID}}
	}
}

func TestConnectorConsentRequiresTheDisplayedAccount(t *testing.T) {
	f := setupWithWorker(t, false)
	alice := f.login("alice@example.com")
	bob := f.login("bob@example.com")
	shared := consentProject(t, f, alice, "shared")
	joinConsentProject(t, f, shared.ID, alice, bob)
	params := connectorAuthorizeParams(f, connectorClient(t, f), auth.Token())
	page := connectorForm(f, "GET", "/oauth/authorize", alice, params)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "alice@example.com") {
		t.Fatal("expected Alice's consent page")
	}
	action, fields, _ := consentControls(t, page)
	fields.Set("namespace", shared.Namespace)
	res := connectorForm(f, "POST", action, bob, fields)
	if res.Code != 409 || res.Header().Get("Location") != "" || !strings.Contains(res.Body.String(), "account changed") || !strings.Contains(res.Body.String(), "bob@example.com") {
		t.Fatal("account switch must require fresh consent even for a shared project", res.Code)
	}
	action, fields, options := consentControls(t, res)
	if !slices.Contains(options, shared.Namespace) {
		t.Fatal("shared project missing on Bob's consent page")
	}
	fields.Set("namespace", shared.Namespace)
	if res := connectorForm(f, "POST", action, bob, fields); res.Code != 303 {
		t.Fatal("Bob could not consent from the new form", res.Code)
	}
}

func TestConnectorCodeExchangeRechecksProject(t *testing.T) {
	f := setupWithWorker(t, false)
	owner := f.login("owner@example.com")
	human := f.login("member@example.com")
	a := consentProject(t, f, human, "alpha")
	b := consentProject(t, f, owner, "shared")
	joinConsentProject(t, f, b.ID, owner, human)
	_, form := connectorAuthCode(t, f, connectorClient(t, f), human, a.ID, b.ID)
	access, _ := connectorTokenPair(t, f, form)
	codeA := connectorCode(t, f, access, a.Namespace)
	codeB := connectorCode(t, f, access, b.Namespace)
	// The user still belongs to A; only the issuing grant has lost that selection.
	if err := f.db.Update(func(st *model.State) error {
		key := auth.Hash(access)
		grant := st.OAuthTokens[key]
		grant.ProjectIDs = []string{b.ID}
		st.OAuthTokens[key] = grant
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if status := f.request("DELETE", "/api/projects/"+b.ID+"/members/member@example.com", owner, nil, nil); status != 200 {
		t.Fatal("remove member", status)
	}
	expectProjectDenied(t, f, access, a.Namespace)
	expectProjectDenied(t, f, access, b.Namespace)
	for _, code := range []string{codeA, codeB} {
		body := map[string]string{"code": code, "name": "test-host"}
		if status := f.request("POST", "/api/agent/login", "", body, nil); status != 404 {
			t.Fatal("code exchange ignored selection or current membership", status)
		}
		if status := f.request("POST", "/api/agent/login", "", body, nil); status != 401 {
			t.Fatal("rejected code was not consumed", status)
		}
	}
}

func TestConnectorCodeKeepsItsLifetimeAcrossAccessExpiry(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	access := connectorAccessToken(t, f, human, p.ID)
	code := connectorCode(t, f, access, p.Namespace)
	if err := f.db.Update(func(st *model.State) error {
		key := auth.Hash(access)
		grant := st.OAuthTokens[key]
		grant.Expires = time.Now().Add(-time.Second)
		st.OAuthTokens[key] = grant
		prune(st)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if status := f.request("POST", "/mcp", access, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, nil); status != 401 {
		t.Fatal("expired access token authenticated", status)
	}
	if status := f.request("POST", "/api/agent/login", "", map[string]string{"code": code, "name": "test-host"}, nil); status != 200 {
		t.Fatal("unexpired code lost its selection", status)
	}
}

func TestConnectorCredentialsWithoutProjectSelectionRequireConsent(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	client := connectorClient(t, f)
	_, form := connectorAuthCode(t, f, client, human, p.ID)
	access, refresh := connectorTokenPair(t, f, form)
	code := connectorCode(t, f, access, p.Namespace)
	pending, pendingForm := connectorAuthCode(t, f, client, human, p.ID)
	// These are the records written before project consent existed.
	if err := f.db.Update(func(st *model.State) error {
		for _, raw := range []string{access, refresh} {
			key := auth.Hash(raw)
			grant := st.OAuthTokens[key]
			grant.ProjectIDs = nil
			st.OAuthTokens[key] = grant
		}
		authCode := st.OAuthCodes[auth.Hash(pending)]
		authCode.ProjectIDs = nil
		st.OAuthCodes[auth.Hash(pending)] = authCode
		login := st.LoginCodes[auth.Hash(code)]
		login.GrantKey = ""
		st.LoginCodes[auth.Hash(code)] = login
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if status := f.request("POST", "/mcp", access, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, nil); status != 401 {
		t.Fatal("unscoped OAuth access token authenticated", status)
	}
	for _, request := range []url.Values{pendingForm, {"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {refresh}}} {
		if res := connectorForm(f, "POST", "/oauth/token", "", request); res.Code != 400 || !strings.Contains(res.Body.String(), "invalid_grant") {
			t.Fatal("exchanged a grant without project consent", res.Code)
		}
	}
	if status := f.request("POST", "/api/agent/login", "", map[string]string{"code": code, "name": "test-host"}, nil); status != 404 {
		t.Fatal("exchanged a login code without a grant binding", status)
	}
}

func TestConnectorToolsSeeOnlyGrantedProjects(t *testing.T) {
	f := setupWithWorker(t, false)
	owner := f.login("owner@example.com")
	human := f.login("member@example.com")
	granted := consentProject(t, f, human, "granted")
	shared := consentProject(t, f, owner, "shared")
	joinConsentProject(t, f, shared.ID, owner, human)
	deleting := consentProject(t, f, human, "deleting")
	if status := f.request("POST", "/api/projects/"+shared.ID+"/put", owner, model.PutRequest{File: &model.FileInput{Name: "secret.md", Content: testSourceContent(testSourceURL("secret.md", "ungranted text"), "ungranted text")}}, nil); status != 202 {
		t.Fatal("prepare ungranted source", status)
	}
	if err := f.db.Update(func(st *model.State) error {
		p := st.Projects[deleting.ID]
		p.Deleting = true
		st.Projects[p.ID] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	access := connectorAccessToken(t, f, human, granted.ID)
	var projects struct {
		Projects []struct{ Namespace string } `json:"projects"`
	}
	if msg := connectorTool(t, f, access, "projects_list", nil, &projects); msg != "" || len(projects.Projects) != 1 || projects.Projects[0].Namespace != granted.Namespace {
		t.Fatal("projects_list showed an ungranted project", msg, projects)
	}
	var sourcesBefore int
	if err := f.db.View(func(st model.State) error { sourcesBefore = len(st.Inbox); return nil }); err != nil {
		t.Fatal(err)
	}
	// The user is a member of both, so only the grant can deny them. A deleting
	// project outside the grant must not reveal its state through a 409 message.
	for _, namespace := range []string{shared.Namespace, deleting.Namespace} {
		for _, tc := range []struct {
			name string
			args map[string]any
		}{
			{"wiki_read", map[string]any{"project": namespace}},
			{"wiki_search", map[string]any{"project": namespace, "query": "ungranted"}},
			{"source_put", map[string]any{"project": namespace, "name": "record.md", "content": "text"}},
			{"cli_login_code", map[string]any{"project": namespace}},
		} {
			if msg := connectorTool(t, f, access, tc.name, tc.args, nil); msg != "project not found" {
				t.Fatal("ungranted project reached a tool", tc.name, namespace, msg)
			}
		}
	}
	if err := f.db.View(func(st model.State) error {
		if len(st.Inbox) != sourcesBefore || len(st.LoginCodes) != 0 {
			t.Error("a rejected tool call changed state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if msg := connectorTool(t, f, access, "wiki_search", map[string]any{"project": granted.Namespace, "query": "anything"}, nil); msg != "" {
		t.Fatal("granted project denied", msg)
	}
}

func TestConnectorConsentLimitsSelectedProjects(t *testing.T) {
	f := setupWithWorker(t, false)
	alice := f.login("alice@example.com")
	p := consentProject(t, f, alice, "alpha")
	params := connectorAuthorizeParams(f, connectorClient(t, f), auth.Token())
	action, fields, _ := consentControls(t, connectorForm(f, "GET", "/oauth/authorize", alice, params))
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = p.Namespace
	}
	fields["namespace"] = ids
	if res := connectorForm(f, "POST", action, alice, fields); res.Code != 400 || res.Header().Get("Location") != "" {
		t.Fatal("too many selected projects accepted", res.Code)
	}
}
