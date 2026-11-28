package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests must never overwrite the enclosing Claude Code session's selection.
func TestMain(m *testing.M) {
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	os.Exit(m.Run())
}

func sessionTestEnv(t *testing.T) string {
	t.Helper()
	home := projectRunHome(t)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-1")
	return home
}

func sessionTestState(now time.Time) *sessionState {
	return &sessionState{Server: defaultServer, Namespace: "org/p", Mode: "local", Path: "/tmp/project", LastPullAt: now, LastPullResult: sessionPullSuccess}
}

func mustSession(t *testing.T) *sessionState {
	t.Helper()
	path, err := sessionPath("session-1")
	if err != nil {
		t.Fatal(err)
	}
	s, err := readSession(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSessionContextFormat(t *testing.T) {
	now := time.Date(2026, 11, 11, 11, 40, 0, 0, time.UTC)
	s := sessionTestState(now)
	want := "Kioku: org/p（local, server=https://kioku.magicians.ee, P=\"/tmp/project\", pull 2026-11-11T11:40:00Z 成功）。再選択と再 pull は不要。"
	if got := sessionContext(s); got != want {
		t.Fatalf("%q != %q", got, want)
	}
	s.LastPullResult = sessionPullFailure
	line := sessionContext(s)
	want = "Kioku: org/p（local, server=https://kioku.magicians.ee, P=\"/tmp/project\", pull 2026-11-11T11:40:00Z 失敗）。最後の pull は失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。"
	if line != want {
		t.Fatal(line)
	}
	s.LastPullAt, s.LastPullResult = time.Time{}, sessionPullNone
	want = "Kioku: org/p（local, server=https://kioku.magicians.ee, P=\"/tmp/project\", pull 未実行）。再選択は不要。最初の pull と Wiki の参照を行う。"
	if got := sessionContext(s); got != want {
		t.Fatal(got)
	}
	s.Path = "/tmp/two  spaces\nquote\""
	if got := sessionContext(s); strings.Contains(got, "\n") || !strings.Contains(got, "two  spaces\\nquote\\\"") {
		t.Fatal(got)
	}
}

func TestSessionStoragePathsRetentionAndPermissions(t *testing.T) {
	home := sessionTestEnv(t)
	path, _ := sessionPath("session-1")
	if want := filepath.Join(home, ".local/state/kioku/sessions/session-1.json"); path != want {
		t.Fatal(path)
	}
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	path, _ = sessionPath("session-1")
	if path != filepath.Join(base, "kioku/sessions/session-1.json") {
		t.Fatal(path)
	}
	now := time.Now().UTC()
	if err := saveSession("session-1", now, sessionTestState(now)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	for _, name := range []string{"old.json", "recent.json", "keep.txt"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("broken"), 0600); err != nil {
			t.Fatal(err)
		}
		age := 31 * 24 * time.Hour
		if name == "recent.json" {
			age = 29 * 24 * time.Hour
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveSession("session-1", now, mustSession(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, name := range []string{"recent.json", "keep.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for p, mode := range map[string]os.FileMode{dir: 0700, path: 0600} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal(info, err)
		}
	}
	if _, err := readSession(path, now.Add(sessionRetention+time.Second)); err == nil {
		t.Fatal("expired state accepted")
	}
	for _, sid := range []string{"", "../escape", "/absolute", "x/y", "x\n", strings.Repeat("a", 101)} {
		if _, err := sessionPath(sid); err == nil {
			t.Fatal("unsafe ID", sid)
		}
	}
}

func TestSessionPullRecordsSuccessFailureAndNoID(t *testing.T) {
	sessionTestEnv(t)
	a, _, _, status := syncFixture(t)
	if err := a.pull(); err != nil {
		t.Fatal(err)
	}
	s := mustSession(t)
	if s.LastPullResult != sessionPullSuccess || s.Namespace != a.Config.Namespace || s.Path != a.Dir {
		t.Fatal(s)
	}
	*status = 403
	if err := a.pull(); err == nil {
		t.Fatal("expected failure")
	}
	s = mustSession(t)
	if s.LastPullResult != sessionPullFailure || !strings.Contains(sessionContext(s), "最後の pull は失敗") {
		t.Fatal(s)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	path, _ := sessionPath("session-1")
	before, _ := os.ReadFile(path)
	_ = a.pull()
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("wrote state without ID")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-1")
	t.Setenv("XDG_STATE_HOME", path) // A file prevents state writes, but not the pull.
	*status = 200
	var stderr bytes.Buffer
	a.Err = &stderr
	if err := a.pull(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "warning: could not update session state") {
		t.Fatal(stderr.String())
	}
}

func TestSessionProjectCommands(t *testing.T) {
	for _, op := range []string{"create", "join"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failure=%v", op, fail), func(t *testing.T) {
				sessionTestEnv(t)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if fail {
						http.Error(w, `{"error":"denied"}`, 403)
						return
					}
					fmt.Fprint(w, `{"namespace":"org/p"}`)
				}))
				defer server.Close()
				args := []string{"--server", server.URL, "-C", t.TempDir()}
				if op == "create" {
					args = append(args, "-n", "org/p")
				}
				args = append(args, "project", op)
				if op == "join" {
					args = append(args, "invite")
				}
				_, _, err := runProjectCLI(args...)
				if (err != nil) != fail {
					t.Fatal(err)
				}
				path, _ := sessionPath("session-1")
				if fail {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatal("failed project command created state", err)
					}
					a := &App{Config: Config{Server: defaultServer, Namespace: "org/previous"}, Dir: "/tmp/previous"}
					a.recordSessionPull(nil)
					before, _ := os.ReadFile(path)
					info, _ := os.Stat(path)
					if _, _, err := runProjectCLI(args...); err == nil {
						t.Fatal("expected failure")
					}
					after, _ := os.ReadFile(path)
					updated, _ := os.Stat(path)
					if !bytes.Equal(before, after) || !info.ModTime().Equal(updated.ModTime()) {
						t.Fatal("failed project command changed state")
					}
					return
				}
				s := mustSession(t)
				if s.Server != server.URL || s.Namespace != "org/p" || s.Mode != "local" || s.Path != args[3] || !s.LastPullAt.IsZero() || s.LastPullResult != sessionPullNone {
					t.Fatal(s)
				}
				data, _ := os.ReadFile(path)
				var fields map[string]any
				if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 4 {
					t.Fatalf("unexpected selection fields: %s (%v)", data, err)
				}
			})
		}
	}
}

func TestSessionSelectionPreservesOnlySameProjectPull(t *testing.T) {
	for _, state := range []string{"missing", "server", "namespace", "path", "same"} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("I5_I6_background/%s/failure=%v", state, failed), func(t *testing.T) {
				sessionTestEnv(t)
				now := time.Now().UTC()
				old := sessionTestState(now)
				switch state {
				case "server":
					old.Server = "https://other.example"
				case "namespace":
					old.Namespace = "org/other"
				case "path":
					old.Path = "/tmp/other"
				}
				if state != "missing" {
					if err := saveSession("session-1", now, old); err != nil {
						t.Fatal(err)
					}
				}
				path, _ := sessionPath("session-1")
				before, _ := os.ReadFile(path)
				info, _ := os.Stat(path)
				a := &App{backgroundPull: true, Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/tmp/project"}
				var pullErr error
				want := sessionPullSuccess
				if failed {
					pullErr = errors.New("failed")
					want = sessionPullFailure
				}
				a.recordSessionPull(pullErr)
				if state == "same" {
					got := mustSession(t)
					if got.LastPullResult != want || got.LastPullAt.Before(now) {
						t.Fatal(got)
					}
				} else {
					after, _ := os.ReadFile(path)
					updated, _ := os.Stat(path)
					if !bytes.Equal(before, after) || (info != nil && !info.ModTime().Equal(updated.ModTime())) {
						t.Fatal("background pull overwrote selection")
					}
				}
			})
		}
	}

	sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/tmp/project"}
	a.recordSessionPull(nil)
	before := mustSession(t).LastPullAt
	a.recordSession(sessionPullNone)
	s := mustSession(t)
	if s.LastPullResult != sessionPullSuccess || !s.LastPullAt.Equal(before) {
		t.Fatal(s)
	}
	a.Config.Namespace = "org/other"
	a.recordSession(sessionPullNone)
	s = mustSession(t)
	if s.LastPullResult != sessionPullNone || !s.LastPullAt.IsZero() {
		t.Fatal(s)
	}
}

func TestSessionConcurrentUpdates(t *testing.T) {
	sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/tmp/project"}
	a.recordSessionPull(nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.recordSessionPull(nil)
		}(i)
	}
	wg.Wait()
	s := mustSession(t)
	if s.LastPullResult != sessionPullSuccess || s.Namespace != "org/p" {
		t.Fatal(s)
	}
}

func TestSessionUnreadableStatePreservesSelection(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions")
	}
	sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/tmp/project"}
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
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })
	var stderr bytes.Buffer
	a.Err = &stderr
	a.Config.Namespace = "org/other"
	a.recordSessionPull(nil)
	if !strings.Contains(stderr.String(), "warning: could not update session state:") {
		t.Error("unreadable state did not produce a warning", stderr.String())
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !info.ModTime().Equal(updated.ModTime()) {
		t.Fatal("unreadable state lost selection")
	}
}

func TestSessionCredentialFailureUpdatesPull(t *testing.T) {
	home := sessionTestEnv(t)
	dir := t.TempDir()
	a := &App{Dir: dir, AuthDir: filepath.Join(home, ".config/kioku/auth"), Config: Config{Server: defaultServer, Namespace: "org/p"}}
	if err := os.MkdirAll(a.metadataDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.saveProjectConfig(); err != nil {
		t.Fatal(err)
	}
	a.recordSessionPull(nil)
	if err := os.MkdirAll(filepath.Dir(a.credentialPath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.credentialPath(), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), []string{"-C", dir, "pull"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected credential failure")
	}
	if s := mustSession(t); s.LastPullResult != sessionPullFailure {
		b, _ := json.Marshal(s)
		t.Fatal(string(b))
	}
}

func TestSessionFailedPullPreservesSelection(t *testing.T) {
	for _, changed := range []string{"missing", "server", "namespace", "path", "same"} {
		t.Run(changed, func(t *testing.T) {
			sessionTestEnv(t)
			a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/tmp/project"}
			path, _ := sessionPath("session-1")
			if changed != "missing" {
				a.recordSessionPull(nil)
			}
			before, _ := os.ReadFile(path)
			switch changed {
			case "server":
				a.Config.Server = "https://example.com"
			case "namespace":
				a.Config.Namespace = "org/other"
			case "path":
				a.Dir = "/tmp/other"
			}
			a.recordSessionPull(errors.New("pull failed"))
			if changed == "missing" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("failed initial pull created state", err)
				}
			} else if changed == "same" {
				s := mustSession(t)
				if s.LastPullResult != sessionPullFailure || s.LastPullAt.IsZero() {
					t.Fatal(s)
				}
			} else {
				after, _ := os.ReadFile(path)
				if !bytes.Equal(before, after) {
					t.Fatal("failed pull replaced selection")
				}
			}
		})
	}
}

func TestSessionMtimeControlsExpiryAndRefresh(t *testing.T) {
	sessionTestEnv(t)
	now := time.Now().UTC()
	oldPull := now.Add(-2 * sessionRetention)
	if err := saveSession("session-1", now, sessionTestState(oldPull)); err != nil {
		t.Fatal(err)
	}
	path, _ := sessionPath("session-1")
	// An old pull remains valid while the session file is recent.
	if _, err := readSession(path, now); err != nil {
		t.Fatal(err)
	}
	oldMtime := now.Add(-sessionRetention + time.Hour)
	if err := os.Chtimes(path, oldMtime, oldMtime); err != nil {
		t.Fatal(err)
	}
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/tmp/project"}
	a.recordSession(sessionPullNone)
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().After(oldMtime) {
		t.Fatal("CLI selection did not refresh mtime", info, err)
	}
	if s, err := readSession(path, now.Add(2*time.Hour)); err != nil || !s.LastPullAt.Equal(oldPull) {
		t.Fatal("CLI selection changed pull time or failed to renew retention", s, err)
	}
	data, _ := os.ReadFile(path)
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 6 {
		t.Fatalf("unexpected state fields: %s (%v)", data, err)
	}
	for _, key := range []string{"server", "namespace", "mode", "path", "last_pull_at", "last_pull_result"} {
		if _, ok := fields[key]; !ok {
			t.Fatal("missing field", key)
		}
	}
	expired := now.Add(-sessionRetention - time.Second)
	if err := os.Chtimes(path, expired, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := readSession(path, now); err == nil {
		t.Fatal("expired mtime accepted")
	}
}

func TestSessionLastPullValidation(t *testing.T) {
	sessionTestEnv(t)
	now := time.Now().UTC()
	for _, tc := range []struct {
		name   string
		at     time.Time
		result sessionPullResult
		valid  bool
	}{
		{"no pull", time.Time{}, "", true},
		{"success", now, "success", true},
		{"failure", now, "failure", true},
		{"missing result", now, "", false},
		{"missing time", time.Time{}, "success", false},
		{"invalid result", now, "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sessionTestState(now)
			s.LastPullAt, s.LastPullResult = tc.at, tc.result
			if err := saveSession("session-1", now, s); err != nil {
				t.Fatal(err)
			}
			path, _ := sessionPath("session-1")
			if _, err := readSession(path, now); (err == nil) != tc.valid {
				t.Fatal("unexpected validation result", err)
			}
		})
	}
}

func TestPullFlagErrorsPreserveStateAndCredentialFailurePrecedesFlags(t *testing.T) {
	for _, brokenCredential := range []bool{false, true} {
		for _, flags := range [][]string{{"--help"}, {"--unknown"}, {"unexpected"}} {
			t.Run(fmt.Sprintf("broken=%v/flags=%v", brokenCredential, flags), func(t *testing.T) {
				home := sessionTestEnv(t)
				a := &App{Dir: t.TempDir(), AuthDir: filepath.Join(home, ".config/kioku/auth"), Config: Config{Server: defaultServer, Namespace: "org/p"}}
				if err := os.MkdirAll(a.metadataDir(), 0700); err != nil {
					t.Fatal(err)
				}
				if err := a.saveProjectConfig(); err != nil {
					t.Fatal(err)
				}
				a.recordSessionPull(nil)
				path, _ := sessionPath("session-1")
				before, _ := os.ReadFile(path)
				info, _ := os.Stat(path)
				if brokenCredential {
					if err := os.MkdirAll(filepath.Dir(a.credentialPath()), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(a.credentialPath(), []byte("broken"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				out, stderr, err := runProjectCLI(append([]string{"-C", a.Dir, "pull"}, flags...)...)
				if brokenCredential {
					if err == nil || out != "" || stderr != "" || mustSession(t).LastPullResult != sessionPullFailure {
						t.Fatal("credential failure must precede flag handling", out, stderr, err)
					}
				} else {
					if (err == nil) != (flags[0] == "--help") {
						t.Fatal(err)
					}
					after, _ := os.ReadFile(path)
					updated, _ := os.Stat(path)
					if !bytes.Equal(before, after) || !info.ModTime().Equal(updated.ModTime()) {
						t.Fatal("flag-only invocation changed session state")
					}
				}
			})
		}
	}
}

func TestProjectSelectionStateFailureWarnsOnceWithoutFailingOperation(t *testing.T) {
	for _, op := range []string{"create", "join"} {
		t.Run(op, func(t *testing.T) {
			sessionTestEnv(t)
			blocker := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(blocker, nil, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_STATE_HOME", blocker)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"namespace":"org/p"}`)
			}))
			defer server.Close()
			dir := t.TempDir()
			args := []string{"--server", server.URL, "-C", dir}
			if op == "create" {
				args = append(args, "-n", "org/p")
			}
			args = append(args, "project", op)
			if op == "join" {
				args = append(args, "invite")
			}
			out, stderr, err := runProjectCLI(args...)
			if err != nil || out != "org/p\n"+dir+"\n" || strings.Count(stderr, "warning: could not update session state:") != 1 {
				t.Fatal(out, stderr, err)
			}
		})
	}
}
