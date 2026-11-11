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
	return &sessionState{Server: defaultServer, Namespace: "org/p", Mode: "local", Path: "/tmp/project", Operation: "pull", Result: "success", UpdatedAt: now, LastPullAt: now, LastPullResult: "success"}
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
	s.Result, s.LastPullResult = "failure", "failure"
	line := sessionContext(s)
	if !strings.Contains(line, "pull 2026-11-11T11:40:00Z 失敗") || strings.Contains(line, "再 pull は不要") {
		t.Fatal(line)
	}
	s.Operation, s.Result, s.LastPullAt, s.LastPullResult = "project create", "success", time.Time{}, ""
	if got := sessionContext(s); !strings.Contains(got, "pull 未実行") || !strings.Contains(got, "最初の pull") {
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
	if err := updateSession("session-1", now, func(*sessionState) *sessionState { return sessionTestState(now) }); err != nil {
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
	if err := updateSession("session-1", now, func(s *sessionState) *sessionState { return s }); err != nil {
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

func TestSessionContextIgnoresMissingBrokenAndUnrelatedConfiguration(t *testing.T) {
	sessionTestEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	os.Mkdir(filepath.Join(dir, ".kioku"), 0700)
	os.WriteFile(filepath.Join(dir, ".kioku/config"), []byte("broken"), 0600)
	t.Setenv("KIOKU_AGENT_TOKEN", "obsolete")
	t.Setenv("KIOKU_SERVER", "invalid")
	path, _ := sessionPath("session-1")
	os.MkdirAll(filepath.Dir(path), 0700)
	for _, data := range []string{"", "broken", "{}", "null", `{"namespace":"org/p"}`} {
		if data != "" {
			os.WriteFile(path, []byte(data), 0600)
		}
		out, stderr, err := runProjectCLI("session", "context", "--session", "session-1")
		if err != nil || out != "" || stderr != "" {
			t.Fatal(out, stderr, err)
		}
	}
	if err := updateSession("session-1", time.Now(), func(*sessionState) *sessionState { return sessionTestState(time.Now()) }); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runProjectCLI("-C", "/does/not/exist", "session", "context", "--session", "session-1")
	if err != nil || stderr != "" || !strings.HasPrefix(out, "Kioku: org/p") {
		t.Fatal(out, stderr, err)
	}
	if _, _, err := runProjectCLI("session", "skip", "https://example.com"); err == nil {
		t.Fatal("stage two command exists")
	}
}

func TestSessionPullRecordsSuccessFailureAndNoID(t *testing.T) {
	sessionTestEnv(t)
	a, _, _, status := syncFixture(t)
	if err := a.pull(); err != nil {
		t.Fatal(err)
	}
	s := mustSession(t)
	if s.LastPullResult != "success" || s.Namespace != a.Config.Namespace || s.Path != a.Dir {
		t.Fatal(s)
	}
	*status = 403
	if err := a.pull(); err == nil {
		t.Fatal("expected failure")
	}
	s = mustSession(t)
	if s.LastPullResult != "failure" || !strings.Contains(sessionContext(s), "pull 失敗") {
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
				s := mustSession(t)
				if s.Operation != "project "+op || (s.Result == "failure") != fail || s.LastPullResult != "" {
					t.Fatal(s)
				}
				if op == "join" && fail {
					if s.Namespace != "" || sessionContext(s) != "" {
						t.Fatal("invented invite namespace", s)
					}
				} else if s.Namespace != "org/p" {
					t.Fatal(s)
				}
			})
		}
	}
}

func TestSessionPreservesNotificationAcrossSelections(t *testing.T) {
	sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/tmp/project"}
	a.recordSession("pull", nil)
	when := time.Now().UTC()
	if err := updateSession("session-1", when, func(s *sessionState) *sessionState {
		s.NotifiedURLs = []string{"https://example.com"}
		return s
	}); err != nil {
		t.Fatal(err)
	}
	a.recordSession("project create", errors.New("already bound"))
	s := mustSession(t)
	if s.LastPullResult != "success" || len(s.NotifiedURLs) == 0 {
		t.Fatal(s)
	}
	a.Config.Namespace = "org/other"
	a.recordSession("project create", nil)
	s = mustSession(t)
	if s.LastPullResult != "" || len(s.NotifiedURLs) != 1 {
		t.Fatal(s)
	}
}

func TestSessionConcurrentUpdates(t *testing.T) {
	sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/tmp/project"}
	a.recordSession("pull", nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				a.recordSession("pull", nil)
				return
			}
			err := updateSession("session-1", time.Now(), func(s *sessionState) *sessionState {
				s.NotifiedURLs = []string{"https://example.com"}
				return s
			})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	s := mustSession(t)
	if len(s.NotifiedURLs) == 0 || s.LastPullResult != "success" {
		t.Fatal(s)
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
	a.recordSession("pull", nil)
	if err := os.MkdirAll(filepath.Dir(a.credentialPath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.credentialPath(), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), []string{"-C", dir, "pull"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected credential failure")
	}
	if s := mustSession(t); s.LastPullResult != "failure" {
		b, _ := json.Marshal(s)
		t.Fatal(string(b))
	}
}
