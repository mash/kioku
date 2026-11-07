package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
)

func TestConnectorAgentLoginStoresSeparateCredentialAndPinsServer(t *testing.T) {
	const token = "kioku_agent.connector-id.secret"
	const code = "kioku_code.single-use"
	calls := 0
	loginCalls := 0
	lastAuth := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		lastAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/api/agent/login" {
			if r.Header.Get("Authorization") != "" {
				t.Error("exchange sent an existing credential")
			}
			var in struct{ Code, Name, Namespace string }
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Code != code || in.Name == "" {
				t.Error("bad code exchange", err)
			}
			if loginCalls > 0 && in.Namespace != "acme/platform" {
				t.Error("bound cwd namespace not sent")
			}
			loginCalls++
			json.NewEncoder(w).Encode(map[string]any{"id": "connector-id", "token": token, "role": "read", "expires": time.Now().Add(24 * time.Hour), "namespace": "acme/platform"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"agent_id": "connector-id"})
	}))
	defer server.Close()
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KIOKU_SERVER", server.URL)
	t.Setenv("KIOKU_AGENT_TOKEN", "")
	authDir := filepath.Join(home, ".config", "kioku", "auth")
	humanPath := filepath.Join(authDir, auth.Hash(server.URL)+".json")
	seedTrustedOrigin(t, humanPath, server.URL)
	human, _ := json.Marshal(credentials{Server: server.URL, Token: "human-credential"})
	if err := os.WriteFile(humanPath, human, 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	run := func(args ...string) error {
		return Run(context.Background(), append([]string{"-C", dir}, args...), &out, &stderr)
	}
	path := filepath.Join(home, ".config", "kioku", "agents", auth.Hash(server.URL), "acme", "platform.json")
	if err := run("agent", "login", code); err == nil || err.Error() != "a human login exists; run kioku logout first to use connector login" {
		t.Fatal("agent login accepted beside a human credential", err)
	}
	if _, err := os.Stat(path); calls != 0 || !os.IsNotExist(err) {
		t.Fatal("refused agent login sent a request or wrote a credential", calls, err)
	}
	human, _ = json.Marshal(credentials{Server: server.URL, Token: ""})
	if err := os.WriteFile(humanPath, human, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run("agent", "login", code); err != nil || stderr.Len() != 0 {
		t.Fatal(err, stderr.String())
	}
	if strings.Contains(out.String()+stderr.String(), token) || strings.Contains(out.String()+stderr.String(), code) {
		t.Fatal("secret printed")
	}
	var printed map[string]any
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil || len(printed) != 4 || printed["id"] != "connector-id" || printed["namespace"] != "acme/platform" {
		t.Fatal("wrong login output", err, out.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved agentCredentials
	if err = json.Unmarshal(data, &saved); err != nil || saved.Token != token || saved.Server != server.URL || saved.Namespace != "acme/platform" {
		t.Fatal("credential not saved", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential permissions", err)
	}
	config, err := os.ReadFile(filepath.Join(dir, ".kioku", "config"))
	var binding map[string]any
	if err != nil || json.Unmarshal(config, &binding) != nil || len(binding) != 2 || binding["server"] != server.URL || binding["namespace"] != "acme/platform" || bytes.Contains(config, []byte(token)) {
		t.Fatal("project binding contains wrong fields", err)
	}
	out.Reset()
	if err := run("agent", "login", code); err != nil || stderr.Len() != 0 {
		t.Fatal("bound cwd login", err, stderr.String())
	}
	out.Reset()
	if err := run("whoami"); err != nil || !strings.Contains(out.String(), "connector-id") || lastAuth != "Bearer "+token {
		t.Fatal("stored identity", err, out.String(), lastAuth)
	}
	before := calls
	if err := run("--server", "https://other.example", "whoami"); err == nil {
		t.Fatal("server override accepted")
	}
	saved.Server = "https://other.example"
	bad, _ := json.Marshal(saved)
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run("whoami"); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatal("stored server mismatch accepted", err)
	}
	if calls != before {
		t.Fatal("request made despite server mismatch")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run("logout"); err != nil || lastAuth != "Bearer "+token {
		t.Fatal(err, lastAuth)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stored agent survived logout", err)
	}
	if actual, err := os.ReadFile(humanPath); err != nil || !bytes.Equal(actual, human) {
		t.Fatal("human credential changed", err)
	}
	if strings.Contains(out.String()+stderr.String(), "human-credential") {
		t.Fatal("human credential printed")
	}
}

func TestConnectorLogoutClearsExpiredOrRevokedCredential(t *testing.T) {
	for _, state := range []string{"expired", "revoked"} {
		t.Run(state, func(t *testing.T) {
			const token = "kioku_agent.connector-id.secret"
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/api/logout" || r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("logout used wrong credential or route")
				}
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]string{"error": "login required"})
			}))
			defer server.Close()
			home, dir := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("KIOKU_SERVER", server.URL)
			t.Setenv("KIOKU_AGENT_TOKEN", "")
			a := &App{AuthDir: filepath.Join(home, ".config", "kioku", "auth"), Dir: dir, Config: Config{Server: server.URL, Namespace: "acme/platform", Token: ""}}
			if err := a.saveCredentials(); err != nil {
				t.Fatal(err)
			}
			human, err := os.ReadFile(a.credentialPath())
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := a.localLock()
			if err != nil {
				t.Fatal(err)
			}
			if err := a.saveProjectConfig(); err != nil {
				t.Fatal(err)
			}
			unlock()
			path := a.agentCredentialPath()
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(time.Hour)
			if state == "expired" {
				expires = time.Now().Add(-time.Hour)
			}
			data, _ := json.Marshal(agentCredentials{Server: server.URL, ID: "connector-id", Token: token, Role: "read", Expires: expires, Namespace: "acme/platform"})
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := Run(context.Background(), []string{"-C", dir, "logout"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) || calls != 1 {
				t.Fatal("inactive agent not logged out", err, calls)
			}
			if actual, err := os.ReadFile(a.credentialPath()); err != nil || !bytes.Equal(actual, human) {
				t.Fatal("human credential changed", err)
			}
		})
	}
}
