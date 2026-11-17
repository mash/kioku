package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

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
	// an oversized malformed input into hook output.
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

func TestSessionHookE25URLSources(t *testing.T) {
	for _, tt := range []struct {
		name, event, want string
		fields            map[string]any
	}{
		{"E11_cwd", "UserPromptSubmit", testTurnRule("org/p", "/work/p", defaultServer), map[string]any{"prompt": "続けて", "cwd": "/work/https://a.example/1"}},
		{"E11_other_stdin_fields", "UserPromptSubmit", testTurnRule("org/p", "/work/p", defaultServer), map[string]any{"prompt": "続けて", "transcript_path": "https://a.example/transcript", "extra": []any{map[string]any{"url": "https://a.example/nested"}}}},
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
				if err != nil {
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
	if got != fmt.Sprintf(listHeaderText, testKiokuCommand(defaultServer))+"\n- org/p: （説明なし）［ヒント: プロンプトに namespace］" {
		t.Fatal(got)
	}
}

func TestStopAndSessionEndHook(t *testing.T) {
	t.Run("R4_bounded_queue_count", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "queue.tsv")
		for _, tc := range []struct {
			body      string
			min, want int
		}{{"\n\n", 5, 0}, {"row\n\nother", 5, 2}, {"row\nother\nthird", 1, 1}, {strings.Repeat("x", 9<<20), 5, 1}} {
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := countQueueRows(path, tc.min)
			if err != nil || got != tc.want {
				t.Fatal(got, err)
			}
		}
	})
	t.Run("R6_detached_child", func(t *testing.T) {
		cmd := exec.Command("/usr/bin/true")
		if err := startProcess(cmd); err != nil {
			t.Fatal(err)
		}
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
			t.Fatal("child was not detached")
		}
	})

	for _, tc := range []struct {
		name, event, cwd, state, server string
		rows                            int
		age                             time.Duration
		child, launch, fail             bool
	}{
		{name: "E13_stop", event: "Stop", rows: 5, launch: true},
		{name: "E37_end", event: "SessionEnd", rows: 1, launch: true},
		{name: "E38_declined", event: "Stop", rows: 7, state: "declined"},
		{name: "E13_below_threshold", event: "Stop", rows: 4},
		{name: "E13_busy", event: "Stop", rows: 5, age: 10 * time.Minute},
		{name: "E13_stale", event: "Stop", rows: 5, age: 40 * time.Minute, launch: true},
		{name: "E4_child", event: "Stop", rows: 5, child: true},
		{name: "E13_unselected", event: "Stop", rows: 5, state: "missing"},
		{name: "E37_empty_cwd", event: "SessionEnd", rows: 1, cwd: "empty"},
		{name: "E37_relative_cwd", event: "SessionEnd", rows: 1, cwd: "relative"},
		{name: "I13_other_server", event: "Stop", rows: 5, server: "https://other.example", launch: true},
		{name: "E13_launch_failure", event: "Stop", rows: 5, launch: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionTestEnv(t)
			root := t.TempDir()
			cwd := t.TempDir()
			if tc.cwd == "empty" {
				cwd = ""
			}
			if tc.cwd == "relative" {
				cwd = "relative"
			}
			s := sessionTestState(time.Now())
			s.Path = root
			s.Namespace = "org/a"
			if tc.server != "" {
				s.Server = tc.server
			}
			if tc.state == "declined" {
				s = &sessionState{DeclinedAt: time.Now().UTC()}
			}
			statePath, _ := sessionPath("session-1")
			if tc.state != "missing" {
				writeHookFixture(t, statePath, s)
			}
			before, _ := os.ReadFile(statePath)
			info, _ := os.Stat(statePath)
			if err := os.MkdirAll(filepath.Join(root, ".kioku"), 0700); err != nil {
				t.Fatal(err)
			}
			var rows []queueRow
			for i := range tc.rows {
				rows = append(rows, queueRow{"2026-11-18", fmt.Sprintf("https://x.example/%d", i), "read"})
			}
			queue := filepath.Join(root, ".kioku/queue.tsv")
			if err := os.WriteFile(queue, formatQueue(rows), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.age != 0 {
				writeHookFixture(t, filepath.Join(root, ".kioku/capture.lock"), captureLock{PID: 123, StartedAt: time.Now().Add(-tc.age)})
			}
			logPath := filepath.Join(root, ".kioku/capture.log")
			if err := os.WriteFile(logPath, []byte("previous"), 0644); err != nil {
				t.Fatal(err)
			}
			oldStart := startProcess
			t.Cleanup(func() { startProcess = oldStart })
			var commands []*exec.Cmd
			startProcess = func(c *exec.Cmd) error {
				commands = append(commands, c)
				if tc.fail {
					return errors.New("failed")
				}
				return nil
			}
			t.Setenv("PATH", t.TempDir())
			t.Setenv("KIOKU_SERVER", "https://wrong.example")
			input := map[string]any{"session_id": "session-1", "hook_event_name": tc.event, "cwd": cwd}
			if tc.child {
				input["agent_id"] = "child"
			}
			data, _ := json.Marshal(input)
			var out, stderr bytes.Buffer
			a := &App{Out: &out, Err: &stderr}
			if err := a.sessionHook([]string{"--event", tc.event, "--prompts", "../../prompts"}, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 || stderr.Len() != 0 {
				t.Fatal(out.String(), stderr.String())
			}
			after, _ := os.ReadFile(statePath)
			if !bytes.Equal(before, after) {
				t.Fatal("hook changed selection")
			}
			if info != nil {
				updated, _ := os.Stat(statePath)
				if !info.ModTime().Equal(updated.ModTime()) {
					t.Fatal("refreshed state")
				}
			}
			if tc.launch {
				if len(commands) != 1 {
					t.Fatal(commands)
				}
				c := commands[0]
				exe, _ := os.Executable()
				want := []string{exe}
				if tc.server != "" {
					want = append(want, "--server", tc.server)
				}
				min := "5"
				if tc.event == "SessionEnd" {
					min = "1"
				}
				pluginRoot, _ := filepath.Abs("../..")
				want = append(want, "-C", s.Path, "-n", "org/a", "capture", "run", "--min", min, "--plugin-root", pluginRoot)
				if !reflect.DeepEqual(c.Args, want) || c.Dir != cwd || c.Stdin != nil || c.Stdout != c.Stderr || c.Stdout.(*os.File).Name() != logPath {
					t.Fatal(c)
				}
				for _, value := range c.Env {
					if strings.HasPrefix(value, "KIOKU_SERVER=") || strings.HasPrefix(value, "CLAUDE_CODE_SESSION_ID=") {
						t.Fatal(value)
					}
				}
				logInfo, err := os.Stat(logPath)
				if err != nil || logInfo.Size() != 0 || logInfo.Mode().Perm() != 0600 {
					t.Fatal(logInfo, err)
				}
			} else {
				if len(commands) != 0 {
					t.Fatal(commands)
				}
				data, _ := os.ReadFile(logPath)
				if string(data) != "previous" {
					t.Fatal("silent hook changed log")
				}
			}
			data, _ = os.ReadFile(queue)
			if !bytes.Equal(data, formatQueue(rows)) {
				t.Fatal("hook changed queue")
			}
		})
	}
}
