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
)

const (
	orderHumanToken = "human-token"
	orderAgentToken = "kioku_agent.connector-id.secret"
	orderNamespace  = "acme/platform"
)

// authRecorder is a test server that records each request's path and
// Authorization header.
type authRecorder struct {
	*httptest.Server
	paths, auths []string
	status       int
}

func newAuthRecorder(t *testing.T) *authRecorder {
	rec := &authRecorder{status: 200}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.paths = append(rec.paths, r.Method+" "+r.URL.Path)
		rec.auths = append(rec.auths, r.Header.Get("Authorization"))
		switch {
		case r.URL.Path == "/api/agent/login":
			json.NewEncoder(w).Encode(map[string]any{"id": "connector-id", "token": orderAgentToken, "role": "read", "expires": time.Now().Add(24 * time.Hour), "namespace": orderNamespace})
		case r.URL.Path == "/api/auth/loopback":
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "bad request"})
		default:
			w.WriteHeader(rec.status)
			json.NewEncoder(w).Encode(map[string]string{"user_id": "u"})
		}
	}))
	t.Cleanup(rec.Close)
	return rec
}

// orderFixture prepares HOME, KIOKU_SERVER and a working directory for Run.
type orderFixture struct {
	t        *testing.T
	server   *authRecorder
	dir      string
	app      *App
	out, err bytes.Buffer
}

func newOrderFixture(t *testing.T) *orderFixture {
	server := newAuthRecorder(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KIOKU_SERVER", server.URL)
	t.Setenv("KIOKU_AGENT_TOKEN", "")
	f := &orderFixture{t: t, server: server, dir: t.TempDir()}
	f.app = &App{AuthDir: filepath.Join(home, ".config", "kioku", "auth"), Config: Config{Server: server.URL, Namespace: orderNamespace}}
	f.app.Dir = f.dir
	return f
}

func (f *orderFixture) run(args ...string) error {
	f.out.Reset()
	f.err.Reset()
	return Run(context.Background(), append([]string{"-C", f.dir}, args...), &f.out, &f.err)
}

func (f *orderFixture) writeHuman(token string) {
	f.t.Helper()
	human := *f.app
	human.Config.Token = token
	if err := human.saveCredentials(); err != nil {
		f.t.Fatal(err)
	}
}

func (f *orderFixture) humanToken() string {
	f.t.Helper()
	var c credentials
	b, err := os.ReadFile(f.app.credentialPath())
	if err != nil || json.Unmarshal(b, &c) != nil {
		f.t.Fatal("read human credential", err)
	}
	return c.Token
}

func (f *orderFixture) bind() {
	f.t.Helper()
	unlock, err := f.app.localLock()
	if err != nil {
		f.t.Fatal(err)
	}
	defer unlock()
	if err := f.app.saveProjectConfig(); err != nil {
		f.t.Fatal(err)
	}
}

// writeAgent writes the agent credential file as valid, expired or damaged.
func (f *orderFixture) writeAgent(state string) string {
	f.t.Helper()
	path := f.app.agentCredentialPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if state == "expired" {
		expires = time.Now().Add(-time.Hour)
	}
	data, _ := json.Marshal(agentCredentials{Server: f.app.Config.Server, ID: "connector-id", Token: orderAgentToken, Role: "read", Expires: expires, Namespace: orderNamespace})
	if state == "damaged" {
		data = []byte("{not json")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func TestHumanCredentialWinsOverAgentCredential(t *testing.T) {
	for _, state := range []string{"valid", "expired", "damaged"} {
		t.Run(state, func(t *testing.T) {
			f := newOrderFixture(t)
			f.writeHuman(orderHumanToken)
			f.bind()
			f.writeAgent(state)
			if err := f.run("whoami"); err != nil {
				t.Fatal(err)
			}
			if len(f.server.auths) != 1 || f.server.auths[0] != "Bearer "+orderHumanToken {
				t.Fatal("whoami did not send the human credential", f.server.auths)
			}
		})
	}
}

func TestExpiredAgentCredentialNeedsAgentLogin(t *testing.T) {
	f := newOrderFixture(t)
	f.bind()
	path := f.writeAgent("expired")
	if err := f.run("whoami"); err == nil || !strings.Contains(err.Error(), "run kioku agent login") {
		t.Fatal("expired agent credential accepted", err)
	}
	if len(f.server.paths) != 0 {
		t.Fatal("request sent with an expired agent credential", f.server.paths)
	}
	if err := f.run("agent", "login", "kioku_code.single-use"); err != nil || f.err.Len() != 0 {
		t.Fatal(err, f.err.String())
	}
	if len(f.server.auths) != 1 || f.server.auths[0] != "" {
		t.Fatal("code exchange sent a credential", f.server.auths)
	}
	var saved agentCredentials
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &saved) != nil || !saved.Expires.After(time.Now()) {
		t.Fatal("agent login did not replace the expired credential", err)
	}
	if err := f.run("whoami"); err != nil || f.server.auths[1] != "Bearer "+orderAgentToken {
		t.Fatal("new agent credential not used", err, f.server.auths)
	}
}

func TestLogoutHumanKeepsAgentCredential(t *testing.T) {
	f := newOrderFixture(t)
	f.writeHuman(orderHumanToken)
	f.bind()
	path := f.writeAgent("valid")
	if err := f.run("logout"); err != nil {
		t.Fatal(err)
	}
	if len(f.server.paths) != 1 || f.server.paths[0] != "POST /api/logout" || f.server.auths[0] != "Bearer "+orderHumanToken {
		t.Fatal("logout did not revoke the human credential", f.server.paths, f.server.auths)
	}
	if f.humanToken() != "" {
		t.Fatal("human token kept after logout")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("human logout removed the agent credential", err)
	}
}

func TestLogoutAcceptsRevokedHumanSession(t *testing.T) {
	f := newOrderFixture(t)
	f.server.status = 401
	f.writeHuman(orderHumanToken)
	if err := f.run("logout"); err != nil {
		t.Fatal(err)
	}
	if len(f.server.auths) != 1 || f.server.auths[0] != "Bearer "+orderHumanToken {
		t.Fatal("logout did not send the human credential", f.server.auths)
	}
	if f.humanToken() != "" {
		t.Fatal("human token kept after a 401")
	}
}

func TestDamagedHumanCredentialAllowsOnlyLogin(t *testing.T) {
	f := newOrderFixture(t)
	f.writeHuman(orderHumanToken)
	if err := os.WriteFile(f.app.credentialPath(), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	loadErr := (&App{AuthDir: f.app.AuthDir, Config: Config{Server: f.app.Config.Server}}).loadCredentials()
	if loadErr == nil {
		t.Fatal("damaged human credential loaded")
	}
	for _, args := range [][]string{{"whoami"}, {"agent", "login", "kioku_code.single-use"}} {
		if err := f.run(args...); err == nil || err.Error() != loadErr.Error() {
			t.Fatal("damaged human credential accepted", args, err)
		}
	}
	if len(f.server.paths) != 0 {
		t.Fatal("request sent with a damaged human credential", f.server.paths)
	}
	if err := f.run("login"); err == nil {
		t.Fatal("login ignored the 400 reply")
	}
	if len(f.server.paths) != 1 || f.server.paths[0] != "POST /api/auth/loopback" || f.server.auths[0] != "" {
		t.Fatal("login did not start the loopback flow without a credential", f.server.paths, f.server.auths)
	}
}

func TestLoginSendsNoStoredHumanCredential(t *testing.T) {
	f := newOrderFixture(t)
	f.writeHuman(orderHumanToken)
	if err := f.run("login"); err == nil {
		t.Fatal("login ignored the 400 reply")
	}
	if len(f.server.paths) != 1 || f.server.paths[0] != "POST /api/auth/loopback" || f.server.auths[0] != "" {
		t.Fatal("login sent a stored credential", f.server.paths, f.server.auths)
	}
}

func TestAgentTokenEnvironmentRejected(t *testing.T) {
	f := newOrderFixture(t)
	f.writeHuman(orderHumanToken)
	t.Setenv("KIOKU_AGENT_TOKEN", "x")
	var out bytes.Buffer
	for _, args := range [][]string{{"version"}, {"help"}, {"-h"}, {"-C", f.dir, "help"}} {
		if err := Run(context.Background(), args, &out, &out); err != nil {
			t.Fatal("bare command rejected", args, err)
		}
	}
	for _, args := range [][]string{{"version"}, {"whoami"}, {"pull"}} {
		if err := f.run(args...); err == nil || err.Error() != "KIOKU_AGENT_TOKEN is no longer supported; unset it and use kioku agent login" {
			t.Fatal("KIOKU_AGENT_TOKEN accepted", args, err)
		}
	}
	if len(f.server.paths) != 0 {
		t.Fatal("request sent with KIOKU_AGENT_TOKEN", f.server.paths)
	}
}

func TestLogoutOutsideBoundDirectory(t *testing.T) {
	f := newOrderFixture(t)
	f.writeHuman(orderHumanToken)
	if err := f.run("logout"); err != nil {
		t.Fatal(err)
	}
	if len(f.server.auths) != 1 || f.server.auths[0] != "Bearer "+orderHumanToken {
		t.Fatal("logout did not send the human credential", f.server.auths)
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".kioku")); !os.IsNotExist(err) {
		t.Fatal("logout created .kioku outside a bound directory", err)
	}
	if err := f.run("logout"); err == nil || err.Error() != "not logged in" {
		t.Fatal("logout without a credential", err)
	}
	if len(f.server.paths) != 1 {
		t.Fatal("logout without a credential sent a request", f.server.paths)
	}
}

func TestAgentCredentialCannotSaveHumanCredential(t *testing.T) {
	dir := t.TempDir()
	a := &App{kind: credentialAgent, AuthDir: dir, Config: Config{Server: "https://x.example"}}
	if err := a.saveCredentials(); err == nil || !strings.Contains(err.Error(), "cannot update human") {
		t.Fatal("agent credential saved human state", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("human credential file written", entries)
	}
}
