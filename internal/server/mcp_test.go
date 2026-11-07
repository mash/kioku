package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func connectorRPC(t *testing.T, f *fixture, token, method string, params any) json.RawMessage {
	t.Helper()
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	status := f.request("POST", "/mcp", token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, &response)
	if status != 200 || response.Error != nil {
		t.Fatal("MCP call failed", status, response.Error)
	}
	return response.Result
}

func connectorCode(t *testing.T, f *fixture, token, namespace string) string {
	t.Helper()
	raw := connectorRPC(t, f, token, "tools/call", map[string]any{"name": "cli_login_code", "arguments": map[string]string{"project": namespace}})
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.IsError || len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatal("login tool failed", err, string(raw))
	}
	var value struct {
		Code    string    `json:"code"`
		Expires time.Time `json:"expires_at"`
		Command string    `json:"command"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &value); err != nil || value.Code == "" || value.Command != "kioku agent login "+value.Code || !value.Expires.After(time.Now()) || value.Expires.After(time.Now().Add(time.Minute)) {
		t.Fatal("invalid login code result", err)
	}
	return value.Code
}

func TestConnectorLoginEndToEnd(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	if status := f.seedSource("POST", "/api/projects/"+project.ID+"/put", human, model.PutRequest{File: &model.FileInput{Name: "evidence.md", Content: "Connector test source", SourceURL: testSourceURL("evidence.md", "Connector test source")}}, nil); status != 202 {
		t.Fatal("prepare snapshot", status)
	}
	access := connectorAccessToken(t, f, human, project.ID)
	raw := connectorRPC(t, f, access, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test", "version": "1"}})
	var init struct {
		ProtocolVersion string            `json:"protocolVersion"`
		Capabilities    map[string]any    `json:"capabilities"`
		ServerInfo      map[string]string `json:"serverInfo"`
	}
	if err := json.Unmarshal(raw, &init); err != nil || init.ProtocolVersion != "2025-06-18" || init.Capabilities["tools"] == nil || init.ServerInfo["name"] != "kioku" {
		t.Fatal("initialize", err, string(raw))
	}
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(connectorRPC(t, f, access, "tools/list", map[string]any{}), &list); err != nil || len(list.Tools) != 5 {
		t.Fatal("tool list", err, list)
	}
	for i, name := range []string{"cli_login_code", "projects_list", "wiki_read", "wiki_search", "source_put"} {
		if list.Tools[i].Name != name {
			t.Fatal("tool order", list)
		}
	}
	code := connectorCode(t, f, access, project.Namespace)
	var issued struct {
		ID        string    `json:"id"`
		Token     string    `json:"token"`
		Namespace string    `json:"namespace"`
		Role      string    `json:"role"`
		Expires   time.Time `json:"expires"`
	}
	if status := f.request("POST", "/api/agent/login", "", map[string]string{"code": code, "name": "test-host"}, &issued); status != 200 {
		t.Fatal("code exchange", status)
	}
	if issued.ID == "" || issued.Token == "" || issued.Namespace != project.Namespace || issued.Role != "read" || time.Until(issued.Expires) < 23*time.Hour || time.Until(issued.Expires) > 24*time.Hour {
		t.Fatal("incorrect credential metadata")
	}
	var snapshot model.Snapshot
	if status := f.request("GET", "/api/projects/"+project.ID+"/snapshot", issued.Token, nil, &snapshot); status != 200 || len(snapshot.Files) == 0 {
		t.Fatal("agent snapshot", status)
	}
	if status := f.request("POST", "/mcp", issued.Token, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"}, nil); status != 401 {
		t.Fatal("agent authenticated MCP", status)
	}
	if status := f.request("POST", "/api/agent/login", "", map[string]string{"code": code, "name": "another-host"}, nil); status != 401 {
		t.Fatal("login code reused", status)
	}
	code = connectorCode(t, f, access, project.Namespace)
	var replacement struct {
		Token string `json:"token"`
	}
	if status := f.request("POST", "/api/agent/login", "", map[string]string{"code": code, "name": "test-host"}, &replacement); status != 200 {
		t.Fatal("replacement", status)
	}
	if status := f.request("GET", "/api/me", issued.Token, nil, nil); status != 401 {
		t.Fatal("old credential not revoked", status)
	}
	if status := f.request("GET", "/api/me", replacement.Token, nil, nil); status != 200 {
		t.Fatal("replacement unusable", status)
	}
}

func TestConnectorCodeExpiryMembershipAndRevocation(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	access := connectorAccessToken(t, f, human, project.ID)
	other := f.login("outsider@example.com")
	otherAccess := connectorAccessToken(t, f, other, securityProject(t, f, other).ID)
	raw := connectorRPC(t, f, otherAccess, "tools/call", map[string]any{"name": "cli_login_code", "arguments": map[string]string{"project": project.Namespace}})
	var denied struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &denied); err != nil || !denied.IsError {
		t.Fatal("non-member received login code", err)
	}
	code := connectorCode(t, f, access, project.Namespace)
	if err := f.db.Update(func(st *model.State) error {
		value := st.LoginCodes[auth.Hash(code)]
		value.Expires = time.Now().Add(-time.Second)
		st.LoginCodes[auth.Hash(code)] = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if status := f.request("POST", "/api/agent/login", "", map[string]string{"code": code, "name": "test-host"}, nil); status != 401 {
		t.Fatal("expired code accepted", status)
	}
	if status := f.request("POST", "/api/account/revoke-all", human, nil, nil); status != 200 {
		t.Fatal("revoke-all", status)
	}
	if status := f.request("POST", "/mcp", access, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, nil); status != 401 {
		t.Fatal("OAuth access survived revoke-all", status)
	}
}

func TestConnectorBindingMismatchPreservesExistingAgent(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	access := connectorAccessToken(t, f, human, project.ID)
	code := connectorCode(t, f, access, project.Namespace)
	var issued struct {
		Token string `json:"token"`
	}
	if status := f.request("POST", "/api/agent/login", "", map[string]string{"code": code, "name": "test-host"}, &issued); status != 200 {
		t.Fatal(status)
	}
	code = connectorCode(t, f, access, project.Namespace)
	if status := f.request("POST", "/api/agent/login", "", map[string]string{"code": code, "name": "test-host", "namespace": "other/project"}, nil); status != 409 {
		t.Fatal("binding mismatch accepted", status)
	}
	if status := f.request("GET", "/api/me", issued.Token, nil, nil); status != 200 {
		t.Fatal("rejected binding revoked existing credential", status)
	}
}
