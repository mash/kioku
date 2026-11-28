package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSessionStartIgnoresBlockedCaptureLog(t *testing.T) {
	home := projectRunHome(t)
	a := projectEntryApp(t)
	a.AuthDir = filepath.Join(home, ".config/kioku/auth")
	now := time.Now()
	if err := a.registerQueueSource("https://example.com/page", now); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(a.metadataDir(), "capture.log")
	if err := syscall.Mkfifo(log, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.addProjectEntry(a.Config.Server, a.Config.Namespace, a.Dir, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		// An absent cwd prevents a late launcher after unblocking the old code.
		done <- sessionStartCapture(sessionHookInput{Cwd: filepath.Join(a.Dir, "absent")}, now)
	}()
	select {
	case message := <-done:
		if !strings.Contains(message, "起動に失敗") {
			t.Fatal(message)
		}
	case <-time.After(time.Second):
		f, err := os.OpenFile(log, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		<-done
		t.Fatal("SessionStart blocked opening capture.log without a reader")
	}
}

func TestStopHookIsSilentWhileCaptureRuns(t *testing.T) {
	a := projectEntryApp(t)
	now := time.Now()
	if err := a.registerQueueSource("https://example.com/first", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.beginCapture(now); err != nil {
		t.Fatal(err)
	}
	s := &sessionState{Server: defaultServer, Namespace: a.Config.Namespace, Mode: "local", Path: a.Dir}
	if message := stopHook(sessionHookInput{Event: "Stop", Cwd: a.Dir}, s, now); message != "" {
		t.Fatal(message)
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
	got := runPromptHook(t, map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "prompt": "続けて"})
	if got != testCandidateHeader(defaultServer)+"\n- org/p: （説明なし）" {
		t.Fatal(got)
	}
}

func TestReadHookQueue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.tsv")
	if queued, failed, err := readHookQueueCounts(path); err != nil || queued != 0 || failed != 0 {
		t.Fatal(queued, failed, err)
	}
	data := formatQueue([]queueRow{
		{Date: "2026-11-18", Source: "https://example.com/queued", State: queueQueued},
		{Date: "2026-11-18", Source: "https://example.com/failed", State: queueFailed, Attempts: 2},
	})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if queued, failed, err := readHookQueueCounts(path); err != nil || queued != 1 || failed != 1 {
		t.Fatal(queued, failed, err)
	}
	if err := os.Symlink(path, filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, bytes.Repeat([]byte{'\n'}, 8<<20)...), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"symlink", "fifo", "queue.tsv"} {
		if _, _, err := readHookQueueCounts(filepath.Join(dir, name)); err == nil {
			t.Errorf("readHookQueue accepted %s", name)
		}
	}
}

func TestStopAndSessionEndHook(t *testing.T) {
	t.Run("R6_detached_child", func(t *testing.T) {
		cmd := exec.Command("/usr/bin/true")
		if err := startProcess(cmd); err != nil {
			t.Fatal(err)
		}
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
			t.Fatal("child was not detached")
		}
	})

	a := projectEntryApp(t)
	if msg := stopHook(sessionHookInput{Event: "Stop", Cwd: a.Dir}, nil, time.Now()); msg != "" {
		t.Fatal(msg)
	}
}
