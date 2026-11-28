package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookToolSources(t *testing.T) {
	cwd := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(string(b), err)
		}
	}
	git("init", "-q")
	git("remote", "add", "origin", "git@github.com:example/project.git")
	const artifactURL = "https://claude.ai/artifact/test-fixture"
	const codeArtifact = "https://claude.ai/code/artifact/a897ab72-f9f4-4a60-80af-5be2dffecd79"
	const page = "https://public.example/document"
	pub := fmt.Sprintf(`"Published «Test» at %s (Version 1, version id 1790000000-a1b2)"`, artifactURL)
	for _, tc := range []struct {
		name, input, output, want string
	}{
		{"WebFetch", `{"url":"https://public.example/document","prompt":"not saved"}`, `"full response not saved"`, page},
		{"WebFetch", `{"url":"https://user@example.com/a"}`, "", ""},
		{"Artifact", `{"action":"read","url":"https://claude.ai/artifact/test-fixture","path":"input.html"}`, "", artifactURL},
		{"Artifact", `{"action":"list","url":"https://claude.ai/artifact/test-fixture"}`, pub, ""},
		{"Artifact", `{"file_path":"index.html"}`, pub, artifactURL},
		{"Artifact", `{"action":"publish","file_path":"index.html"}`, pub, artifactURL},
		{"Artifact", `{"action":"publish","url":"https://claude.ai/artifact/same"}`, pub, "https://claude.ai/artifact/same"},
		{"Artifact", `{"action":"read","url":"` + codeArtifact + `","path":"index.html"}`, "", codeArtifact},
		{"Artifact", `{"action":"publish","url":"` + codeArtifact + `"}`, "", codeArtifact},
		{"Artifact", `{"action":"publish"}`, fmt.Sprintf(`"Published Test at %s (Version 2, version id 1790000000-c3d4)"`, codeArtifact), codeArtifact},
		{"Artifact", `{"action":"publish"}`, `{"success":true,"url":"` + codeArtifact + `"}`, ""},
		{"Artifact", `{"action":"publish"}`, `"Error: publish failed"`, ""},
		{"Bash", `{"command":"gh issue view 4"}`, "", "https://github.com/example/project/issues/4"},
		{"Bash", `{"command":"gh pr view -R example/project 4 --json title"}`, "", "https://github.com/example/project/pull/4"},
		{"Bash", `{"command":"gh issue view 4 --repo wrong/project"}`, "", ""},
		{"Bash", `{"command":"gh issue view 4 -R other.github.com/example/project"}`, "", ""},
		{"Bash", `{"command":"gh pr view https://github.com/some/repo/pull/19"}`, "", "https://github.com/some/repo/pull/19"},
		{"Bash", `{"command":"gh api repos/example/project/issues/4"}`, "", ""},
		{"Bash", `{"command":"gh issue view 4 && touch marker"}`, "", ""},
		{"Bash", `{"command":"gh issue view 4 > marker"}`, "", ""},
		{"mcp__claude_ai_Notion__notion-fetch", `{"id":"test","a":{"y":2,"a":1}}`, `"not saved"`, `tool:{"input":{"a":{"a":1,"y":2},"id":"test"},"tool":"mcp__claude_ai_Notion__notion-fetch"}`},
		{"mcp__slack__slack_send_message", `{"channel":"chat"}`, "", ""},
		{"mcp__plugin_kioku_kioku__wiki_read", `{}`, "", ""},
	} {
		in := hookToolEvent{Name: tc.name, Cwd: cwd, Input: json.RawMessage(tc.input), Response: json.RawMessage(tc.output)}
		if got := toolHookSource(t.Context(), in); got != tc.want {
			t.Errorf("%s %s: %q want %q", tc.name, tc.input, got, tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(cwd, "marker")); !os.IsNotExist(err) {
		t.Fatal("executed Bash input", err)
	}
	git("remote", "set-url", "origin", "../some/project")
	if got := githubViewSource(t.Context(), cwd, "gh issue view 4"); got != "" {
		t.Fatal("non-GitHub origin accepted", got)
	}
}

func TestHookToolUsesOnlySelectedWritableProject(t *testing.T) {
	home := sessionTestEnv(t)
	t.Setenv(captureModeEnv, "")
	dir := t.TempDir()
	const origin = "https://fixture.invalid"
	const ns = "org/p"
	a := &App{Dir: dir, AuthDir: filepath.Join(home, ".config/kioku/auth"), Config: Config{Server: origin, Namespace: ns}}
	if err := a.ensureProject(); err != nil {
		t.Fatal(err)
	}
	if err := a.saveProjectConfig(); err != nil {
		t.Fatal(err)
	}
	const result = `{"hook_event_name":"PostToolUse","session_id":"session-1","agent_id":"child","tool_name":"WebFetch","tool_input":{"url":"https://example.com/page"},"tool_response":"body must not be persisted"}`
	invoke := func() { hookTool(t.Context(), strings.NewReader(result)) }
	noQueue := func() {
		t.Helper()
		if _, err := os.Stat(a.queuePath()); !os.IsNotExist(err) {
			t.Fatal("queue mutated", err)
		}
	}
	invoke()
	noQueue()
	s := &sessionState{Mode: "local", Server: origin, Namespace: ns, Path: dir}
	if err := saveSession("session-1", time.Now(), s); err != nil {
		t.Fatal(err)
	}
	invoke()
	noQueue()
	path := a.agentCredentialPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	agent := agentCredentials{Server: origin, Namespace: ns, ID: "fixture", Token: "kioku_agent.fixture.test", Expires: time.Now().Add(time.Hour)}
	saveRole := func(role string) {
		t.Helper()
		agent.Role = role
		data, err := json.Marshal(agent)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	saveRole("read")
	invoke()
	noQueue()
	saveRole("write")
	t.Setenv(captureModeEnv, "1")
	invoke()
	noQueue()
	t.Setenv(captureModeEnv, "")
	before, err := os.ReadFile(mustSessionPath(t, "session-1"))
	if err != nil {
		t.Fatal(err)
	}
	invoke()
	invoke()
	rows, err := readQueue(a.queuePath(), true)
	if err != nil || len(rows) != 1 || rows[0].Source != "https://example.com/page" || rows[0].Attempts != 0 || rows[0].Reason != "" {
		t.Fatal(rows, err)
	}
	data, err := os.ReadFile(a.queuePath())
	if err != nil || strings.Contains(string(data), "body must not be persisted") {
		t.Fatal(string(data), err)
	}
	after, err := os.ReadFile(mustSessionPath(t, "session-1"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("selection changed", err)
	}
}

func mustSessionPath(t *testing.T, sid string) string {
	t.Helper()
	path, err := sessionPath(sid)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHookToolInputLimitDoesNotApplyToResponse(t *testing.T) {
	large := strings.Repeat("x", maxHookToolInputBytes)
	for _, tc := range []struct {
		name, input, response string
		want                  string
		bad                   bool
	}{
		{"WebFetch", `{"url":"https://example.com"}`, `"` + large + `"`, "https://example.com", false},
		{"WebFetch", `{"url":"https://example.com","prompt":"` + large + `"}`, `""`, "", true},
		{"Artifact", `{"action":"publish"}`, `"Published fixture at https://claude.ai/code/artifact/fixture (Version 2, version id 1790000000-c3d4) ` + large + `"`, "https://claude.ai/code/artifact/fixture", false},
	} {
		data := `{"tool_response":` + tc.response + `,"hook_event_name":"PostToolUse","tool_input":` + tc.input + `,"tool_name":"` + tc.name + `"}`
		var event hookToolEvent
		err := decodeHookEvent(strings.NewReader(data), &event)
		if tc.bad {
			if err == nil {
				t.Fatal("oversized tool_input was accepted")
			}
		} else if err != nil || toolHookSource(t.Context(), event) != tc.want ||
			tc.name != "Artifact" && len(event.Response) != 0 {
			t.Fatal(tc.name, len(event.Response), err)
		}
	}
}
