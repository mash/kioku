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
	input := map[string]any{
		"prompt":        "http://ignored.test https://example.com/prompt.",
		"tool_input":    map[string]any{"url": "https://example.com/input"},
		"tool_response": []any{map[string]any{"text": "[one](https://claude.ai/artifact/abc) https://example.com/two"}, `{"url":"https:\/\/example.com\/encoded"}`},
	}
	want := []string{"https://example.com/prompt", "https://example.com/input", "https://claude.ai/artifact/abc", "https://example.com/two", "https://example.com/encoded"}
	if got := collectCaptureURLs(input); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestSessionHookGatesAndDeduplication(t *testing.T) {
	sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/tmp/project"}
	a.recordSession("pull", nil)
	prompts := t.TempDir()
	if err := os.WriteFile(filepath.Join(prompts, "hook-capture.txt"), []byte("Kioku: {{namespace}} {{urls}}"), 0600); err != nil {
		t.Fatal(err)
	}
	base := map[string]any{"session_id": "session-1", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "gh pr create"}, "tool_response": map[string]any{"stdout": "https://github.com/org/repo/pull/42"}}
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
	if !strings.Contains(context, "org/p") || !strings.Contains(context, "https://github.com/org/repo/pull/42") {
		t.Fatal(context)
	}
	if out := run(base); out != "" {
		t.Fatal("duplicate", out)
	}
	base["tool_response"] = map[string]any{"stdout": "https://github.com/org/repo/pull/43"}
	if out := run(base); out == "" {
		t.Fatal("different URL suppressed")
	}
	base["hook_event_name"] = "UserPromptSubmit"
	base["prompt"] = "https://example.com/prompt https://example.com/prompt"
	if out := run(base); strings.Count(out, "https://example.com/prompt") != 1 {
		t.Fatal(out)
	}
	delete(base, "prompt")
	base["hook_event_name"] = "PostToolUse"
	base["tool_name"] = "AnyTool"
	base["tool_input"] = map[string]any{"url": "https://example.com/input"}
	base["tool_response"] = map[string]any{"success": false, "data": []any{"https://example.com/nested"}}
	out = run(base)
	if !strings.Contains(out, "https://example.com/input") || !strings.Contains(out, "https://example.com/nested") {
		t.Fatal(out)
	}
	if out = run(base); out != "" {
		t.Fatal(out)
	}
	base["tool_input"] = map[string]any{}
	// Concurrent notifications for a single URL must emit exactly once.
	base["tool_response"] = map[string]any{"stdout": "https://github.com/org/repo/pull/44"}
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
	base["tool_response"] = many
	if out := run(base); !strings.Contains(out, many[0]) || !strings.Contains(out, many[1000]) {
		t.Fatal("missing new URLs")
	}
	state := mustSession(t)
	if len(state.NotifiedURLs) != 1000 || state.NotifiedURLs[0] != many[1] || state.NotifiedURLs[999] != many[1000] {
		t.Fatal(state.NotifiedURLs)
	}
	base["tool_response"] = many[1000]
	if out := run(base); out != "" {
		t.Fatal(out)
	}
	base["tool_response"] = many[0]
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
	base["tool_response"] = "https://example.com/no-namespace"
	if out := run(base); out != "" {
		t.Fatal(out)
	}

}
