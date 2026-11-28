package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHookToolGuardSnapshot(t *testing.T) {
	a := projectEntryApp(t)
	tool, err := captureToolSource("mcp__claude_ai_Linear__get_issue", json.RawMessage(`{"id":"42"}`))
	if err != nil {
		t.Fatal(err)
	}
	const artifact = "https://claude.ai/artifact/fixture"
	const web = "https://example.com/notes"
	for _, src := range []string{tool, artifact, web} {
		if err := a.registerQueueSource(src, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	lock, _, err := a.beginCapture(time.Now())
	if err != nil || lock == nil {
		t.Fatal(lock, err)
	}
	dir, err := resolveCaptureDir(lock.Token, a.captureTmp(lock.Token))
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dir, "test's file.md")
	if err := os.WriteFile(inside, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	base := shellQuote("/bin/path with space/kioku")
	project := base + " -C " + shellQuote(a.Dir) + " -n org/p"
	t.Setenv(captureModeEnv, "1")
	t.Setenv(captureDirEnv, dir)
	t.Setenv(captureCommandEnv, base)
	t.Setenv(captureProjectCommandEnv, project)
	check := func(name string, args map[string]any, deny bool) {
		t.Helper()
		input, err := json.Marshal(map[string]any{
			"hook_event_name": "PreToolUse", "tool_name": name, "tool_input": args,
		})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := hookToolGuard(bytes.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		if (out.Len() != 0) != deny || (deny && !strings.Contains(out.String(), `"permissionDecision":"deny"`)) {
			t.Fatal(name, args, out.String())
		}
	}
	check("mcp__claude_ai_Linear__get_issue", map[string]any{"id": "42"}, false)
	check("mcp__claude_ai_Linear__get_issue_metadata", map[string]any{"id": "42"}, false)
	check("mcp__claude_ai_Notion__get_issue", map[string]any{"id": "42"}, true)
	check("mcp__claude_ai_Linear__update_issue", map[string]any{"id": "42"}, true)
	check("Artifact", map[string]any{"action": "read", "url": artifact, "path": inside}, false)
	check("Artifact", map[string]any{"action": "read", "url": artifact, "path": outside}, true)
	check("Artifact", map[string]any{"action": "read", "url": artifact + "x"}, true)
	check("Artifact", map[string]any{"action": "publish", "url": artifact}, true)
	check("Artifact", map[string]any{"url": artifact}, true)
	check("Read", map[string]any{"file_path": inside}, false)
	check("Write", map[string]any{"file_path": filepath.Join(dir, "new.md")}, false)
	check("Read", map[string]any{"file_path": outside}, true)
	check("Write", map[string]any{"file_path": link}, true)
	check("Write", map[string]any{"file_path": filepath.Join(dir, "snapshot.tsv")}, true)
	check("Write", map[string]any{"file_path": filepath.Join(dir, "Snapshot.TSV")}, true)
	check("Write", map[string]any{"file_path": filepath.Join(dir, "Instructions.MD")}, true)
	check("Write", map[string]any{"file_path": captureProgressPath(dir)}, true)
	check("Edit", nil, true)
	for _, cmd := range []string{
		base + " capture fetch " + web + " --capture-token " + lock.Token + " --dir " + shellQuote(dir) + " --out " + shellQuote(inside),
		project + " capture put --capture-token " + lock.Token + " --source " + shellQuote(tool) + " " + shellQuote(inside),
		project + " capture result --capture-token " + lock.Token + " --result skipped --reason " + shellQuote("not a document; it isn't text") + " " + web,
	} {
		check("Bash", map[string]any{"command": cmd}, false)
		check("Bash", map[string]any{"command": cmd + "; echo no"}, true)
		check("Bash", map[string]any{"command": cmd + " $(echo no)"}, true)
	}
	check("Bash", map[string]any{"command": project + " capture run"}, true)
	check("Bash", map[string]any{"command": "gh issue view 42"}, true)
	if err := os.WriteFile(filepath.Join(dir, "snapshot.tsv"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	check("mcp__claude_ai_Linear__get_issue", map[string]any{"id": "42"}, true)
	t.Setenv(captureDirEnv, "")
	check("mcp__claude_ai_Linear__get_issue", map[string]any{"id": "42"}, true)
	t.Setenv(captureModeEnv, "")
	check("Artifact", map[string]any{"action": "publish", "url": artifact}, false)
}

func TestCaptureAllowedToolsByServer(t *testing.T) {
	const web = "https://example.com/source"
	tmp := filepath.Join(t.TempDir(), "token")
	webRules := captureAllowedTools("/bin/kioku", "/bin/kioku -C /tmp -n org/p", tmp, []queueRow{{Source: web}})
	for _, rule := range webRules {
		if strings.HasPrefix(rule, "mcp__") || strings.Contains(rule, "gh ") || strings.Contains(rule, "capture run") {
			t.Fatal(webRules)
		}
	}
	tool, err := captureToolSource("mcp__connector__server__get_issue", json.RawMessage(`{"id":42}`))
	if err != nil {
		t.Fatal(err)
	}
	got := captureAllowedTools("/bin/kioku", "/bin/kioku -C /tmp -n org/p", tmp, []queueRow{{Source: tool}, {Source: tool}, {Source: web}})
	if !slices.Contains(webRules, "Edit(/"+tmp+"/**)") {
		t.Fatal(webRules)
	}
	if len(got) != len(webRules)+1 || got[len(got)-1] != "mcp__connector__server__*" {
		t.Fatal(got)
	}
}
