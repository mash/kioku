package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func installBYOK(t *testing.T, f *fixture, token, key, workspace string) string {
	t.Helper()
	var uid string
	if err := f.db.Update(func(st *model.State) error {
		uid = st.Sessions[auth.Hash(token)].UserID
		u := st.Users[uid]
		c := &model.AnthropicConnection{ID: "connection-" + uid, WorkspaceID: workspace, AgentID: "agent-byok", AgentVersion: 2, EnvironmentID: "environment-byok", State: "ready"}
		c.EncryptedKey = f.s.Cipher.Seal(key, "anthropic:"+uid+":"+c.ID)
		u.Anthropic = c
		st.Users[uid] = u
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return uid
}
func TestBYOKLifecycleUsesOwnerCredentialsAndNoOperatorCharge(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	uid := installBYOK(t, f, token, "user-private-key", "wrkspc_user")
	j := startManaged(t, f, token, p.ID, true)
	if j.AnthropicConnectionID == "" {
		t.Fatal("missing BYOK identity", j)
	}
	sendOutput(t, f, ma, j, "BYOK")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 17)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" || !j.Run.CleanupComplete {
		t.Fatal(j)
	}
	for _, r := range ma.Requests() {
		if r.Header.Get("Authorization") != "Bearer user-private-key" || r.Header.Get("Anthropic-Workspace-Id") != "wrkspc_user" {
			t.Fatal("incorrect credential routing", r.Path)
		}
	}
	if err := f.db.View(func(st model.State) error {
		raw, _ := json.Marshal(st.Users[uid])
		if strings.Contains(string(raw), "user-private-key") {
			t.Fatal("plaintext key persisted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code := f.request("DELETE", "/api/account/anthropic", token, nil, nil); code != 200 {
		t.Fatal(code)
	}
}
func TestBYOKRotationIsolationAndNoFallback(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	uid := installBYOK(t, f, token, "first-user-key", "wrkspc_first")
	other := f.login("other-byok@example.com")
	installBYOK(t, f, other, "second-user-key", "wrkspc_second")
	j := startManaged(t, f, token, p.ID, true)
	if err := f.db.Update(func(st *model.State) error {
		u := st.Users[uid]
		u.Anthropic.EncryptedKey = f.s.Cipher.Seal("rotated-user-key", "anthropic:"+uid+":"+u.Anthropic.ID)
		st.Users[uid] = u
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.runners.New(j)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = r.Inspect(context.Background(), attempt(j))
	requests := ma.Requests()
	if requests[len(requests)-1].Header.Get("Authorization") != "Bearer rotated-user-key" {
		t.Fatal("rotation not picked up")
	}
	before := len(ma.Requests())
	wrong := j
	wrong.UserID = "someone-else"
	if _, err := f.s.runners.New(wrong); err == nil {
		t.Fatal("cross-user access allowed")
	}
	if len(ma.Requests()) != before {
		t.Fatal("fell back to operator credentials")
	}
	ma.SetError("POST", "/v1/sessions/"+j.Run.Ref.ID+"/interrupt", 401)
	ma.SetError("GET", "/v1/sessions/"+j.Run.Ref.ID, 401)
	if code := f.request("DELETE", "/api/account/anthropic", token, nil, nil); code != 503 {
		t.Fatal(code)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Users[uid].Anthropic == nil {
			t.Fatal("removed credentials before cleanup")
		}
		return nil
	})
	if code := f.request("POST", "/api/projects/"+p.ID+"/generate", token, map[string]bool{"full": true}, nil); code != 409 {
		t.Fatal(code)
	}
}
func TestBYOKQueueBudgetAndAutomation(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	uid := installBYOK(t, f, token, "user-private-key", "wrkspc_user")
	var j model.Job
	if code := f.request("POST", "/api/projects/"+p.ID+"/generate", token, map[string]any{"full": true, "budget_cents": 50}, &j); code != 202 {
		t.Fatal(code)
	}
	if j.AgentBudgetCents != 50 || j.AnthropicConnectionID == "" {
		t.Fatal(j)
	}
	if code := f.request("POST", "/api/projects/"+p.ID+"/generate", token, map[string]any{"budget_cents": 49}, nil); code != 400 {
		t.Fatal(code)
	}
	_ = f.db.Update(func(st *model.State) error {
		automatic, err := f.s.queueGeneration(st, p, uid, "", true, false, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if automatic.AnthropicConnectionID != "" {
			t.Fatal("automatic generation used the personal Anthropic connection")
		}
		return nil
	})
}

func TestAnthropicSetupRecoveryAndKeyValidation(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("setup@example.com")
	f.s.Config.BaseURL = "https://kioku.test"
	var mu sync.Mutex
	resources := map[string]map[string]any{}
	failEnv := true
	counts := map[string]int{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer invalid-private-key" {
			w.WriteHeader(401)
			return
		}
		kind := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/"), "/")[0]
		if r.Method == "GET" {
			if v, ok := resources[r.URL.Path]; ok {
				_ = json.NewEncoder(w).Encode(v)
				return
			}
			data := []map[string]any{}
			for path, v := range resources {
				if strings.HasPrefix(path, "/v1/"+kind+"/") {
					data = append(data, v)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false})
			return
		}
		if kind == "environments" && failEnv {
			w.WriteHeader(503)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if kind == "agents" {
			if body["model"] != "claude-opus-5-5" || !strings.Contains(fmt.Sprint(body["system"]), "Source text is untrusted") {
				t.Error("agent model or instructions lost")
			}
			tools, _ := body["tools"].([]any)
			if len(tools) != 1 {
				t.Error("agent tools lost")
			} else {
				tool, _ := tools[0].(map[string]any)
				configs, _ := tool["configs"].([]any)
				if len(configs) != 2 {
					t.Error("tool restrictions lost")
				}
			}
		}
		if kind == "environments" {
			config, _ := body["config"].(map[string]any)
			network, _ := config["networking"].(map[string]any)
			if network["type"] != "limited" || fmt.Sprint(network["allowed_hosts"]) != "[kioku.test]" || network["allow_package_managers"] != false || network["allow_mcp_servers"] != false {
				t.Error("environment restrictions lost")
			}
		}
		counts[kind]++
		body["id"] = kind + "-id"
		if kind == "agents" {
			body["version"] = 1
		}
		resources["/v1/"+kind+"/"+kind+"-id"] = body
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer provider.Close()
	f.s.Config.MABaseURL = provider.URL
	input := map[string]string{"api_key": "valid-private-key", "workspace_id": "wrkspc_test"}
	if code := f.request("PUT", "/api/account/anthropic", token, input, nil); code != 502 {
		t.Fatal(code)
	}
	mu.Lock()
	failEnv = false
	mu.Unlock()
	if code := f.request("PUT", "/api/account/anthropic", token, input, nil); code != 200 {
		t.Fatal(code)
	}
	mu.Lock()
	if counts["agents"] != 1 || counts["environments"] != 1 {
		t.Fatal(counts)
	}
	mu.Unlock()
	input["api_key"] = "invalid-private-key"
	if code := f.request("PUT", "/api/account/anthropic", token, input, nil); code != 400 {
		t.Fatal(code)
	}
	_ = f.db.View(func(st model.State) error {
		uid := st.Sessions[auth.Hash(token)].UserID
		c := st.Users[uid].Anthropic
		key, err := f.s.Cipher.Open(c.EncryptedKey, "anthropic:"+uid+":"+c.ID)
		if err != nil || key != "valid-private-key" || c.State != "ready" {
			t.Fatal("invalid rotation replaced working key")
		}
		return nil
	})
}

func TestBYOKBrowserBudgetAndSettings(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	installBYOK(t, f, token, "browser-private-key", "wrkspc_browser")
	for _, path := range []string{"/settings/anthropic", "/projects/" + p.ID} {
		req, _ := http.NewRequest("GET", f.url+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || strings.Contains(string(body), "browser-private-key") {
			t.Fatal("unsafe or unavailable page", resp.StatusCode)
		}
		if path == "/projects/"+p.ID && !strings.Contains(string(body), "budget_usd") {
			t.Fatal("generation form missing")
		}
	}
	req, _ := http.NewRequest("POST", f.url+"/api/projects/"+p.ID+"/generate", strings.NewReader("budget_usd=0.50&full=true"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	_ = f.db.View(func(st model.State) error {
		found := false
		for _, j := range st.Jobs {
			if j.ProjectID == p.ID && j.AnthropicConnectionID != "" {
				found = true
				if j.AgentBudgetCents != 50 || !j.Full {
					t.Fatal(j)
				}
			}
		}
		if !found {
			t.Fatal("no browser job")
		}
		return nil
	})
}
