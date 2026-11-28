package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func TestPutRejectsOldEntryPoints(t *testing.T) {
	projectRunHome(t)
	a := projectEntryApp(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-1")
	claude := t.TempDir()
	t.Setenv("PATH", claude)
	marker := filepath.Join(claude, "ran")
	if err := os.WriteFile(filepath.Join(claude, "claude"), []byte("#!/bin/sh\necho ran > '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	const u = "https://example.com/page"
	if _, err := a.addQueueURLs([]string{u}, "keep", time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(a.queuePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"put", u}, {"put", "--now", u}, {"put", "--note", "note", u},
		{"put", "--date", "2026-11-20", "file:///a.md"},
		{"put", "note.md", "file:///a.md"}, {"put", "--capture-token", "anything", "note.md"},
		{"queue", "add", u}, {"queue", "list"}, {"queue", "remove", u}, {"fetch", u},
	} {
		out, _, runErr := runProjectCLI(append([]string{"-C", a.Dir}, args...)...)
		after, err := os.ReadFile(a.queuePath())
		if err != nil || runErr == nil || out != "" || !bytes.Equal(before, after) {
			t.Fatal(args, out, runErr, err)
		}
		if args[0] == "put" && !strings.Contains(runErr.Error(), putUsage) {
			t.Fatal(args, runErr)
		}
	}
	for _, path := range []string{marker, a.captureLockPath()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal(path, err)
		}
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
