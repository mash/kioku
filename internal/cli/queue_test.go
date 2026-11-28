package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPutHTTPSNowStartsCaptureWhenOutputFails(t *testing.T) {
	a := projectEntryApp(t)
	fakeCaptureClaude(t, "backgrounded · 1a2b3c4d\n", 0)
	out, err := os.CreateTemp(t.TempDir(), "closed-output")
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	a.Out = out
	if err := a.put([]string{"--now", "--plugin-root", t.TempDir(), "https://example.com/page"}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("want closed stdout error, got %v", err)
	}
	lock, err := readCaptureLock(a.captureLockPath())
	if err != nil || lock == nil || lock.BgID != "1a2b3c4d" {
		t.Fatalf("--now did not launch the queued capture: %v, %v", lock, err)
	}
}

func TestCaptureResultUpdatesQueueWhenLogFails(t *testing.T) {
	a := projectEntryApp(t)
	u := "https://example.com/accepted"
	now := time.Now()
	if _, err := a.addQueueURLs([]string{u}, "", now); err != nil {
		t.Fatal(err)
	}
	lock, _, err := a.beginCapture(1, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(a.metadataDir(), "capture.log"), 0700); err != nil {
		t.Fatal(err)
	}
	err = a.captureResult([]string{"--capture-token", lock.Token, "--result", "accepted", "--reason", "HTTP 202", u})
	if err == nil || !strings.Contains(err.Error(), "capture.log") {
		t.Fatalf("want log error, got %v", err)
	}
	rows, err := readQueue(a.queuePath())
	if err != nil || len(rows) != 0 {
		t.Fatalf("confirmed acceptance should remove the row even when logging fails: %v, %v", rows, err)
	}
}

func TestPutHTTPSQueueBatchOrder(t *testing.T) {
	a := projectEntryApp(t)
	now := time.Now()
	const first, second, third = "https://example.com/1", "https://example.com/2", "https://example.com/3"
	if _, err := a.addQueueURLs([]string{first, second}, "old", now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.addQueueURLs([]string{second, third, second}, "new", now); err != nil {
		t.Fatal(err)
	}
	rows, err := readQueue(a.queuePath())
	if err != nil || len(rows) != 3 || rows[0].URL != first || rows[0].Note != "old" ||
		rows[1].URL != third || rows[1].Note != "new" || rows[2].URL != second || rows[2].Note != "new" {
		t.Fatal(rows, err)
	}
}

func BenchmarkAddQueueURLs(b *testing.B) {
	a := &App{Dir: b.TempDir(), Config: Config{Namespace: "org/p"}}
	urls := make([]string, 512)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://example.com/%d", i)
	}
	now := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := a.addQueueURLs(urls, "", now); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPutHTTPSQueue(t *testing.T) {
	home := projectRunHome(t)
	a := &App{Dir: t.TempDir(), AuthDir: filepath.Join(home, ".config/kioku/auth"), Config: Config{Server: defaultServer, Namespace: "org/a"}}
	if err := a.ensureProject(); err != nil {
		t.Fatal(err)
	}
	if err := a.saveProjectConfig(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CLAUDE_PLUGIN_ROOT", "")
	run := func(args ...string) (string, string, error) {
		t.Helper()
		return runProjectCLI(append([]string{"-C", a.Dir}, args...)...)
	}
	const first = "https://example.com/page"
	const second = "https://example.com/other"
	reject := func(want string, args ...string) {
		t.Helper()
		before, _ := os.ReadFile(a.queuePath())
		out, _, err := run(args...)
		after, _ := os.ReadFile(a.queuePath())
		if err == nil || !strings.Contains(err.Error(), want) || out != "" || !bytes.Equal(before, after) {
			t.Fatal(args, out, err, string(before), string(after))
		}
	}
	reject("Claude Code の外", "put", first)
	reject("claude コマンドが無い", "put", "--now", first)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-1")
	if out, stderr, err := run("put", first); out != "queued "+first+"（queue 1 件）\n" || stderr != "" || err != nil {
		t.Fatal(out, stderr, err)
	}
	reject("対応しないスキーム http:", "put", "http://example.com")
	reject("private address blocked", "put", second, "https://127.0.0.1/")
	reject("タブと改行", "put", second, "https://example.com/a\tb")
	reject("usage:", "put", "--plugin-root", "/p", first)
	reject("usage:", "put", "--date", "2026-11-20", first)
	reject("usage:", "put", first, "relative.md")
	reject("usage:", "put", "--capture-token", "anything", "note.md")
	for _, sub := range []string{"add", "list", "remove"} {
		reject("unknown command \"queue\"", "queue", sub, first)
	}
	reject("unknown command \"fetch\"", "fetch", first)
	if out, _, err := run("help"); err != nil || strings.Contains(out, "capture run") || strings.Contains(out, "queue add") || !strings.Contains(out, "https://URL") {
		t.Fatal(out, err)
	}
	if out, _, err := run("put", "--note", "a\tb\nc\rd", first); err != nil || !strings.Contains(out, "queue 1 件") {
		t.Fatal(out, err)
	}
	rows, err := readQueue(a.queuePath())
	if err != nil || len(rows) != 1 || rows[0] != (queueRow{time.Now().Format(time.DateOnly), first, "a b c d", 0, queueQueued, ""}) {
		t.Fatal(rows, err)
	}
	info, err := os.Stat(a.queuePath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	fakeCaptureClaude(t, "backgrounded · 1a2b3c4d\n", 1)
	reject("plugin root が要る", "put", "--now", second)
	out, _, err := run("put", "--now", "--plugin-root", "/p", second)
	if err == nil || !strings.Contains(err.Error(), "claude --bg failed") || !strings.Contains(out, "queue 2 件") {
		t.Fatal(out, err)
	}
	rows, err = readQueue(a.queuePath())
	if err != nil || len(rows) != 2 || rows[0].Attempts != 0 || rows[1].Attempts != 0 {
		t.Fatal(rows, err)
	}
	fakeCaptureClaude(t, "backgrounded · 1a2b3c4d\n", 0)
	out, stderr, err := run("put", "--now", "--plugin-root", "/p", first)
	if err != nil || stderr != "" || !strings.Contains(out, "org/a、2 件、id 1a2b3c4d") || strings.Contains(out, "accepted") {
		t.Fatal(out, stderr, err)
	}
	out, stderr, err = run("put", "--now", "--plugin-root", "/p", first)
	if err != nil || stderr != "" || !strings.Contains(out, "実行中（id 1a2b3c4d）") {
		t.Fatal(out, stderr, err)
	}
	rows, err = readQueue(a.queuePath())
	if err != nil || len(rows) != 2 || rows[0].URL != second || rows[0].Attempts != 1 || rows[1].URL != first || rows[1].Attempts != 0 || rows[1].Note != "kioku put" {
		t.Fatal(rows, err)
	}
}

func TestParseQueueRejectsLegacyAndInvalidRows(t *testing.T) {
	for _, raw := range []string{
		"2026-11-28\thttps://example.com\tnote\n",
		"2026-11-28\thttps://example.com\tnote\t4\tqueued\t\n",
		"2026-11-28\thttps://example.com\tnote\t0\tunknown\t\n",
	} {
		if _, err := parseQueue([]byte(raw)); err == nil || err.Error() != "invalid queue row" {
			t.Fatal(raw, err)
		}
	}
}

func TestStatusShowsLocalStateWithoutServer(t *testing.T) {
	home := projectRunHome(t)
	ts := httptest.NewServer(http.NotFoundHandler())
	ts.Close()
	a := &App{Dir: t.TempDir(), AuthDir: filepath.Join(home, ".config/kioku/auth"), Config: Config{Server: ts.URL, Namespace: "org/a"}}
	if err := a.ensureProject(); err != nil {
		t.Fatal(err)
	}
	if err := a.saveProjectConfig(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.addQueueURLs([]string{"https://example.com/page", "https://example.com/other"}, "note", time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	lock, err := acquireCaptureLock(a.captureLockPath(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.recordCaptureID(a.captureLockPath(), lock.Token, "1a2b3c4d"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runProjectCLI("-C", a.Dir, "status")
	var result struct {
		Queue   []queueRow
		Capture struct {
			ID        string
			StartedAt time.Time `json:"started_at"`
		}
		ServerError string `json:"server_error"`
	}
	if decodeErr := json.Unmarshal([]byte(out), &result); err == nil || decodeErr != nil || len(result.Queue) != 2 || result.Capture.ID != "1a2b3c4d" || !result.Capture.StartedAt.Equal(now) || result.ServerError == "" {
		t.Fatal(out, err, decodeErr)
	}
	if out, _, err := runProjectCLI("-C", a.Dir, "delete", "https://example.com/page", "https://example.com/other"); err == nil || out != "" {
		t.Fatal(out, err)
	}
	if rows, err := readQueue(a.queuePath()); err != nil || len(rows) != 1 || rows[0].URL != "https://example.com/other" {
		t.Fatal(rows, err)
	}
}
