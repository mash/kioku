package cli

import (
	"bytes"
	"encoding/json"
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
	if err := a.registerQueueSource(u, now); err != nil {
		t.Fatal(err)
	}
	lock, _, err := a.beginCapture(now)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(a.metadataDir(), "capture.log"), 0700); err != nil {
		t.Fatal(err)
	}
	err = a.captureResult([]string{"--capture-token", lock.Token, "--result", "skipped", "--reason", "not a document", u})
	if err == nil || !strings.Contains(err.Error(), "capture.log") {
		t.Fatalf("want log error, got %v", err)
	}
	rows, err := readQueue(a.queuePath())
	if err != nil || len(rows) != 0 {
		t.Fatalf("skipped result should remove the row even when logging fails: %v, %v", rows, err)
	}
}

func TestQueueRegistration(t *testing.T) {
	a := projectEntryApp(t)
	now := time.Now()
	const first, second = "https://example.com/a", "https://example.com/b"
	rows := []queueRow{
		{Date: "2026-11-18", Source: first, Attempts: 1, State: queueQueued, Reason: "early failure"},
		{Date: "2026-11-18", Source: second, Attempts: 2, State: queueFailed, Reason: "late failure"},
		{Date: "2026-11-18", Source: "https://example.com/c", State: queueQueued},
	}
	if err := a.ensureProject(); err != nil {
		t.Fatal(err)
	}
	before := formatQueue(rows)
	if err := os.WriteFile(a.queuePath(), before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.registerQueueSource(first, now); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(a.queuePath()); err != nil || !bytes.Equal(got, before) {
		t.Fatal(string(got), err)
	}
	if err := a.registerQueueSource(second, now); err != nil {
		t.Fatal(err)
	}
	got, err := readQueue(a.queuePath())
	want := queueRow{Date: now.Local().Format(time.DateOnly), Source: second, State: queueQueued}
	if err != nil || len(got) != 3 || got[0] != rows[0] || got[1] != rows[2] || got[2] != want {
		t.Fatal(got, err)
	}
	if info, err := os.Stat(a.queuePath()); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
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
	if err := a.registerQueueSource(u, time.Now()); err != nil {
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
		"2026-11-28\thttps://example.com\tnote\t0\tqueued\t\n",
		"2026-11-28\thttps://example.com\t3\tqueued\t\n",
		"2026-11-28\thttps://example.com\tnote\t0\tunknown\t\n",
	} {
		if _, err := parseQueue([]byte(raw)); err == nil || !strings.Contains(err.Error(), "delete .kioku/queue.tsv") {
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
	for _, u := range []string{"https://example.com/page", "https://example.com/other"} {
		if err := a.registerQueueSource(u, time.Now()); err != nil {
			t.Fatal(err)
		}
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
	if rows, err := readQueue(a.queuePath()); err != nil || len(rows) != 1 || rows[0].Source != "https://example.com/other" {
		t.Fatal(rows, err)
	}
}

func TestCaptureResultsReleaseAfterEveryRowEvenIfDeleted(t *testing.T) {
	a := projectEntryApp(t)
	now := time.Now()
	tool, err := captureToolSource("mcp__linear__get_issue", json.RawMessage(`{"id":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	const url = "https://example.com/other"
	for _, src := range []string{tool, url} {
		if err := a.registerQueueSource(src, now); err != nil {
			t.Fatal(err)
		}
	}
	lock, rows, err := a.beginCapture(now)
	if err != nil || lock == nil || len(rows) != 2 {
		t.Fatal(lock, rows, err)
	}
	dir := a.captureTmp(lock.Token)
	snapshotPath := filepath.Join(dir, "snapshot.tsv")
	before, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.deleteSources([]string{tool}); err != nil {
		t.Fatal(err)
	}
	if err := a.registerQueueSource(tool, now); err != nil {
		t.Fatal(err)
	}
	if err := a.capturePut([]string{"--capture-token", lock.Token, "--source", tool, filepath.Join(dir, "source.md")}); err == nil || !strings.Contains(err.Error(), "再登録") {
		t.Fatal(err)
	}
	apply := func(result string, args ...string) error {
		return a.captureResult(append([]string{"--capture-token", lock.Token, "--result", result}, args...))
	}
	if err := apply("skipped", tool); err == nil {
		t.Fatal("skipped must require a reason")
	}
	if err := apply("skipped", "--reason", "numbers only", tool); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(snapshotPath); err != nil || !bytes.Equal(before, after) {
		t.Fatal(string(after), err)
	}
	if err := apply("accepted", url); err != nil {
		t.Fatal(err)
	}
	if read, err := readCaptureLock(a.captureLockPath()); err != nil || read != nil {
		t.Fatal(read, err)
	}
	if err := a.recordCaptureID(a.captureLockPath(), lock.Token, "1a2b3c4d"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(a.metadataDir(), "capture.last")); err != nil || string(data) != "1a2b3c4d\n" {
		t.Fatal(string(data), err)
	}
	if data, err := os.ReadFile(filepath.Join(a.metadataDir(), "capture.log")); err != nil || !strings.Contains(string(data), "numbers only") {
		t.Fatal(string(data), err)
	}
	if remaining, err := readQueue(a.queuePath()); err != nil || len(remaining) != 1 || remaining[0].Source != tool || remaining[0].Attempts != 0 {
		t.Fatal(remaining, err)
	}
}
