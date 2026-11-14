package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestCollectCaptureURLs(t *testing.T) {
	input := "http://ignored.test https://example.com/prompt. [one](https://claude.ai/artifact/abc) https://example.com/two"
	want := []string{"https://example.com/prompt", "https://claude.ai/artifact/abc", "https://example.com/two"}
	if got := collectCaptureURLs(input); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestSessionHookOversizedInputIsSilent(t *testing.T) {
	sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/work/p"}
	a.recordSessionPull(nil)
	path, _ := sessionPath("session-1")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prefix := `{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"https://example.com/doc"}`
	// The prefix alone is valid JSON. Truncating the invalid tail must not turn
	// an oversized malformed input into a notification and a state mutation.
	input := prefix + strings.Repeat(" ", (8<<20)-len(prefix)) + "{"
	var out, stderr bytes.Buffer
	a.Out, a.Err = &out, &stderr
	if err := a.sessionHook([]string{"--prompts", "../../prompts"}, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || stderr.Len() != 0 {
		t.Error("oversized input emitted output")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("oversized input changed state")
	}
}

func TestSessionHookGatesAndDeduplication(t *testing.T) {
	sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/tmp/project"}
	a.recordSessionPull(nil)
	prompts := t.TempDir()
	if err := os.WriteFile(filepath.Join(prompts, "hook-capture.txt"), []byte("Kioku: {{namespace}} {{urls}}"), 0600); err != nil {
		t.Fatal(err)
	}
	base := map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "prompt": "https://example.com/doc/42"}
	run := func(in any) string {
		t.Helper()
		var out bytes.Buffer
		b, _ := json.Marshal(in)
		app := &App{Out: &out, Err: &bytes.Buffer{}}
		if err := app.sessionHook([]string{"--prompts", prompts}, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	for _, id := range []any{"child", "", nil} {
		base["agent_id"] = id
		if out := run(base); out != "" {
			t.Fatal(out)
		}
	}
	delete(base, "agent_id")
	base["session_id"] = "unknown"
	if out := run(base); out != "" {
		t.Fatal(out)
	}
	path, _ := sessionPath("unknown")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	base["session_id"] = "session-1"
	out := run(base)
	var decoded map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatal(err, out)
	}
	context := decoded["hookSpecificOutput"]["additionalContext"]
	if !strings.Contains(context, "org/p") || !strings.Contains(context, "https://example.com/doc/42") {
		t.Fatal(context)
	}
	if out := run(base); out != "" {
		t.Fatal("duplicate", out)
	}
	base["prompt"] = "https://example.com/doc/43"
	if out := run(base); out == "" {
		t.Fatal("different URL suppressed")
	}
	base["prompt"] = "https://example.com/prompt https://example.com/prompt"
	if out := run(base); strings.Count(out, "https://example.com/prompt") != 1 {
		t.Fatal(out)
	}
	// Concurrent notifications for a single URL must emit exactly once.
	base["prompt"] = "https://example.com/doc/44"
	var wg sync.WaitGroup
	results := make(chan string, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- run(base) }()
	}
	wg.Wait()
	close(results)
	count := 0
	for out := range results {
		if out != "" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("notification count", count)
	}
	// Keep only the most recent 1000 distinct notifications across calls.
	var many []string
	for i := 0; i < 1001; i++ {
		many = append(many, fmt.Sprintf("https://example.com/%d", i))
	}
	base["prompt"] = strings.Join(many, " ")
	if out := run(base); !strings.Contains(out, many[0]) || !strings.Contains(out, many[1000]) {
		t.Fatal("missing new URLs")
	}
	state := mustSession(t)
	if len(state.NotifiedURLs) != 1000 || state.NotifiedURLs[0] != many[1] || state.NotifiedURLs[999] != many[1000] {
		t.Fatal(state.NotifiedURLs)
	}
	base["prompt"] = many[1000]
	if out := run(base); out != "" {
		t.Fatal(out)
	}
	base["prompt"] = many[0]
	if out := run(base); out == "" {
		t.Fatal("evicted URL must notify again")
	}
	if len(mustSession(t).NotifiedURLs) != 1000 {
		t.Fatal("cap exceeded")
	}
	path, _ = sessionPath("session-1")
	state = mustSession(t)
	state.Namespace = ""
	data, _ := json.Marshal(state)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	base["prompt"] = "https://example.com/no-namespace"
	if out := run(base); out != "" {
		t.Fatal(out)
	}

}

func TestSessionHookE23ToE25URLSources(t *testing.T) {
	for _, tt := range []struct {
		name, event, want string
		fields            map[string]any
	}{
		{"E23_grep_response", "PostToolUse", "", map[string]any{"tool_name": "Grep", "tool_response": "https://a.example/1"}},
		{"E24_fetch_input", "PostToolUse", "", map[string]any{"tool_name": "WebFetch", "tool_input": map[string]any{"url": "https://a.example/1"}}},
		{"E25_cwd", "UserPromptSubmit", fmt.Sprintf(turnRuleText, "org/p", "/work/p"), map[string]any{"prompt": "続けて", "cwd": "/work/https://a.example/1"}},
		{"other_stdin_fields", "UserPromptSubmit", fmt.Sprintf(turnRuleText, "org/p", "/work/p"), map[string]any{"prompt": "続けて", "transcript_path": "https://a.example/transcript", "extra": []any{map[string]any{"url": "https://a.example/nested"}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sessionTestEnv(t)
			a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/work/p"}
			a.recordSessionPull(nil)
			path, _ := sessionPath("session-1")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			tt.fields["session_id"] = "session-1"
			tt.fields["hook_event_name"] = tt.event
			payload, err := json.Marshal(tt.fields)
			if err != nil {
				t.Fatal(err)
			}
			for _, eventArgs := range [][]string{nil, {"--event", tt.event}} {
				var out, stderr bytes.Buffer
				a := &App{Out: &out, Err: &stderr}
				args := append([]string{"--prompts", "../../prompts"}, eventArgs...)
				err := a.sessionHook(args, bytes.NewReader(payload))
				if tt.event == "PostToolUse" && len(eventArgs) != 0 {
					if err == nil || err.Error() != "usage: kioku session hook [--event EVENT] --prompts DIR" {
						t.Fatal("unsupported --event must return usage", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if stderr.Len() != 0 {
					t.Fatal(stderr.String())
				}
				if tt.want == "" {
					if out.Len() != 0 {
						t.Fatal(out.String())
					}
				} else {
					var decoded map[string]map[string]string
					if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
						t.Fatal(err)
					}
					if got := decoded["hookSpecificOutput"]; got["hookEventName"] != tt.event || got["additionalContext"] != tt.want {
						t.Fatal(got)
					}
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				afterInfo, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) {
					t.Fatal("ignored URLs changed state")
				}
			}
		})
	}
}

func TestReadHookPromptPreservesEmptyMissingAndSymlinkBehavior(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), []byte(" \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readHookPrompt(dir, "empty.txt"); got != "" || err != nil {
		t.Fatal("empty template must remain readable", got, err)
	}
	if _, err := readHookPrompt(dir, "missing.txt"); !os.IsNotExist(err) {
		t.Fatal("missing template must remain distinguishable", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("  context\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(dir, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	if got, err := readHookPrompt(dir, "linked.txt"); got != "context" || err != nil {
		t.Fatal("readable symlink must still work", got, err)
	}
}

func TestHookUnreadableStateStillOffersCandidates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions")
	}
	home := sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/work/p"}
	a.recordSessionPull(nil)
	writeHookFixture(t, filepath.Join(home, ".config/kioku/projects.json"), projectsFile{Projects: []projectEntry{
		{Server: defaultServer, Namespace: "org/p", Path: "/work/p"},
	}})
	path, _ := sessionPath("session-1")
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })
	got := runPromptHook(t, map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "prompt": "org/p https://example.com/doc"})
	if got != fmt.Sprintf(singleCandidateText, "org/p") {
		t.Fatal(got)
	}
}
