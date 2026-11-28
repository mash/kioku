package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	rows, err := readQueue(a.queuePath(), true)
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
	before := bytes.TrimSuffix(formatQueue(rows), []byte("\n"))
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
	got, err := readQueue(a.queuePath(), true)
	want := queueRow{Date: now.Local().Format(time.DateOnly), Source: second, State: queueQueued}
	if err != nil || len(got) != 3 || got[0] != rows[0] || got[1] != rows[2] || got[2] != want {
		t.Fatal(got, err)
	}
	if info, err := os.Stat(a.queuePath()); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
}

func TestCaptureIDOnlyUpdatesMatchingLock(t *testing.T) {
	a := projectEntryApp(t)
	now := time.Now()
	const first = "https://example.com/older"
	old := startTestCapture(t, a, now, first)
	if err := a.captureResult([]string{"--capture-token", old.Token, "--result", "skipped", "--reason", "fixture", first}); err != nil {
		t.Fatal(err)
	}
	if err := a.recordCaptureID(a.captureLockPath(), old.Token, "aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if got, err := readCaptureLock(a.captureLockPath()); err != nil || got != nil {
		t.Fatal(got, err)
	}
	next := startTestCapture(t, a, now, "https://example.com/newer")
	if err := a.recordCaptureID(a.captureLockPath(), next.Token, "bbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	if err := a.recordCaptureID(a.captureLockPath(), old.Token, "aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if got, err := readCaptureLock(a.captureLockPath()); err != nil || got == nil || got.BgID != "bbbbbbbb" {
		t.Fatal(got, err)
	}
}

func startTestCapture(t *testing.T, a *App, now time.Time, sources ...string) *captureLock {
	t.Helper()
	for _, src := range sources {
		if err := a.registerQueueSource(src, now); err != nil {
			t.Fatal(err)
		}
	}
	lock, rows, err := a.beginCapture(now)
	if err != nil || lock == nil || len(rows) != len(sources) {
		t.Fatal(lock, rows, err)
	}
	return lock
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
		{"put", u}, {"put", "-h"}, {"put", "--help"}, {"put", "--now", u}, {"put", "--note", "note", u},
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
		if _, err := parseQueue([]byte(raw), true); err == nil || !strings.Contains(err.Error(), "delete .kioku/queue.tsv") {
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
	if out, _, err := runProjectCLI("-C", a.Dir, "delete", "https://example.com/page", "https://example.com/other"); err == nil ||
		out != "removed https://example.com/page\nremoved https://example.com/other\n" {
		t.Fatal(out, err)
	}
	if rows, err := readQueue(a.queuePath(), true); err != nil || len(rows) != 0 {
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
	lock := startTestCapture(t, a, now, tool, url)
	dir := a.captureTmp(lock.Token)
	snapshotPath := filepath.Join(dir, captureSnapshotFile)
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
	if err := apply("accepted", url); err == nil || !strings.Contains(err.Error(), "HTTP 202") {
		t.Fatal("accepted without any submission", err)
	}
	if err := apply("accepted", "--reason", "unexpected", url); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatal("accepted with a reason", err)
	}
	if err := apply("skipped", "--reason", "not a document", url); err != nil {
		t.Fatal(err)
	}
	if read, err := readCaptureLock(a.captureLockPath()); err != nil || read != nil {
		t.Fatal(read, err)
	}
	if data, err := os.ReadFile(filepath.Join(a.metadataDir(), "capture.log")); err != nil || !strings.Contains(string(data), "numbers only") {
		t.Fatal(string(data), err)
	}
	if remaining, err := readQueue(a.queuePath(), true); err != nil || len(remaining) != 1 || remaining[0].Source != tool || remaining[0].Attempts != 0 {
		t.Fatal(remaining, err)
	}
}

func TestCaptureNoResultExhaustionRetainsLastReason(t *testing.T) {
	for _, firstReason := range []string{"", "sign-in required"} {
		t.Run("first-reason="+firstReason, func(t *testing.T) {
			a := projectEntryApp(t)
			now := time.Now()
			const src = "https://example.com/story"
			if err := a.registerQueueSource(src, now); err != nil {
				t.Fatal(err)
			}
			expire := func() {
				t.Helper()
				if err := updateCaptureLock(a.captureLockPath(), func(lock *captureLock) error {
					if lock == nil {
						return errors.New("expected live capture")
					}
					lock.StartedAt = now.Add(-31 * time.Minute)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			for attempt := 1; attempt <= 2; attempt++ {
				lock, snapshot, err := a.beginCapture(now)
				if err != nil || lock == nil || len(snapshot) != 1 || snapshot[0].Attempts != attempt {
					t.Fatal(lock, snapshot, err)
				}
				if attempt == 1 && firstReason != "" {
					if err := a.captureResult([]string{"--capture-token", lock.Token, "--result", "unsaved", "--reason", firstReason, src}); err != nil {
						t.Fatal(err)
					}
				} else {
					expire()
				}
			}
			if lock, snapshot, err := a.beginCapture(now); err != nil || lock != nil || len(snapshot) != 0 {
				t.Fatal(lock, snapshot, err)
			}
			want := firstReason
			if want == "" {
				want = "capture が 2 回とも結果を返さなかった"
			}
			rows, err := readQueue(a.queuePath(), true)
			if err != nil || len(rows) != 1 || rows[0].Attempts != 2 || rows[0].State != queueFailed || rows[0].Reason != want {
				t.Fatal(rows, err)
			}
			if lock, err := readCaptureLock(a.captureLockPath()); lock != nil || err != nil {
				t.Fatal(lock, err)
			}
		})
	}
}

func TestStatusRejectsOldQueueWithoutModifyingIt(t *testing.T) {
	a := projectEntryApp(t)
	if err := a.ensureProject(); err != nil {
		t.Fatal(err)
	}
	old := []byte("2026-11-28\thttps://example.com/page\told note\n")
	if err := os.WriteFile(a.queuePath(), old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.status(); !errors.Is(err, errQueueFormat) || !strings.Contains(err.Error(), "delete .kioku/queue.tsv") {
		t.Fatal(err)
	}
	if stored, err := os.ReadFile(a.queuePath()); err != nil || !bytes.Equal(stored, old) {
		t.Fatal(string(stored), err)
	}
}

func TestCaptureSubmissionDoesNotBlockNewRegistration(t *testing.T) {
	a := projectEntryApp(t)
	const first, second = "https://example.com/slow", "https://example.com/new"
	lock := startTestCapture(t, a, time.Now(), first)
	dir, _, err := captureSnapshot(lock.Token, a.captureTmp(lock.Token))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "source.md")
	if err := os.WriteFile(path, []byte(SourceContentForTest(first, "evidence")), 0600); err != nil {
		t.Fatal(err)
	}
	entered, respond := make(chan struct{}), make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-respond
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "{}")
	}))
	defer ts.Close()
	release := sync.OnceFunc(func() { close(respond) })
	defer release()
	a.Config.Server, a.Client, a.ctx = ts.URL, ts.Client(), t.Context()
	posted := make(chan error, 1)
	go func() {
		posted <- a.capturePut([]string{"--capture-token", lock.Token, "--source", first, path})
	}()
	select {
	case <-entered:
	case err := <-posted:
		t.Fatal("put did not reach HTTP fixture", err)
	case <-time.After(3 * time.Second):
		t.Fatal("put did not start")
	}
	registered := make(chan error, 1)
	go func() { registered <- a.registerQueueSource(second, time.Now()) }()
	select {
	case err := <-registered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue registration blocked by HTTP put")
	}
	// Deliver 202 before accepting the result. The new row must remain.
	release()
	if err := <-posted; err != nil {
		t.Fatal(err)
	}
	if err := a.captureResult([]string{"--capture-token", lock.Token, "--result", "accepted", first}); err != nil {
		t.Fatal(err)
	}
	rows, err := readQueue(a.queuePath(), true)
	if err != nil || len(rows) != 1 || rows[0].Source != second {
		t.Fatal(rows, err)
	}
}

func TestCapturePutKeepsAcceptedStatusWhenBodyIsTruncated(t *testing.T) {
	a := projectEntryApp(t)
	const src = "https://example.com/accepted"
	lock := startTestCapture(t, a, time.Now(), src)
	dir, _, err := captureSnapshot(lock.Token, a.captureTmp(lock.Token))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "source.md")
	if err := os.WriteFile(file, []byte(SourceContentForTest(src, "evidence")), 0600); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusAccepted)
		w.(http.Flusher).Flush()
	}))
	defer ts.Close()
	var out bytes.Buffer
	a.Config.Server, a.Client, a.ctx, a.Out = ts.URL, ts.Client(), t.Context(), &out
	if err := a.capturePut([]string{"--capture-token", lock.Token, "--source", src, file}); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join(a.metadataDir(), "capture.log"))
	want := "accepted " + src + "\n"
	if err != nil || string(log) != want || out.String() != want {
		t.Fatal(string(log), out.String(), err)
	}
	if err := a.captureResult([]string{"--capture-token", lock.Token, "--result", "accepted", src}); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterQueueRejectsToolSourceOver4096Bytes(t *testing.T) {
	a := projectEntryApp(t)
	now := time.Now()
	const name = "mcp__fixture__get_document"
	encode := func(n int) (string, error) {
		return captureToolSource(name, json.RawMessage(`{"query":"`+strings.Repeat("x", n)+`"}`))
	}
	empty, err := encode(0)
	if err != nil {
		t.Fatal(err)
	}
	n := maxCaptureSourceBytes - len(empty)
	src, err := encode(n)
	if err != nil || len(src) != 4096 {
		t.Fatal(len(src), err)
	}
	if err := a.registerQueueSource(src, now); err != nil {
		t.Fatal(err)
	}
	if _, err := encode(n + 1); err == nil {
		t.Fatal("oversized input produced an identity")
	}
	if err := a.registerQueueSource(src+"x", now); err == nil {
		t.Fatal("registered oversized identity")
	}
	if err := a.deleteSources([]string{"-"}); err == nil {
		t.Fatal("accepted old stdin deletion")
	}
	rows, err := readQueue(a.queuePath(), true)
	if err != nil || len(rows) != 1 || rows[0].Source != src {
		t.Fatal(rows, err)
	}
}

func TestCaptureSecondUnsavedReasonReplacesFirst(t *testing.T) {
	a := projectEntryApp(t)
	now := time.Now()
	const src = "https://example.com/retry"
	if err := a.registerQueueSource(src, now); err != nil {
		t.Fatal(err)
	}
	for i, reason := range []string{"first failure", "second failure"} {
		lock, _, err := a.beginCapture(now)
		if err != nil || lock == nil {
			t.Fatal(lock, err)
		}
		if err := a.captureResult([]string{"--capture-token", lock.Token, "--result", "unsaved", "--reason", reason, src}); err != nil {
			t.Fatal(err)
		}
		want := queueQueued
		if i == 1 {
			want = queueFailed
		}
		rows, err := readQueue(a.queuePath(), true)
		if err != nil || len(rows) != 1 || rows[0].Attempts != i+1 || rows[0].Reason != reason || rows[0].State != want {
			t.Fatal(rows, err)
		}
	}
}
