package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
			rows, err := readQueue(a.queuePath())
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
	if err := a.registerQueueSource(first, time.Now()); err != nil {
		t.Fatal(err)
	}
	lock, _, err := a.beginCapture(time.Now())
	if err != nil || lock == nil {
		t.Fatal(lock, err)
	}
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
	rows, err := readQueue(a.queuePath())
	if err != nil || len(rows) != 1 || rows[0].Source != second {
		t.Fatal(rows, err)
	}
}

func TestDeleteLargeToolSourceFromStdin(t *testing.T) {
	a := projectEntryApp(t)
	source, err := captureToolSource("mcp__fixture__get_document", json.RawMessage(`{"query":"`+strings.Repeat("x", 350<<10)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.registerQueueSource(source, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := a.deleteSourcesFrom([]string{"-"}, strings.NewReader(source+"\n")); err != nil {
		t.Fatal(err)
	}
	if rows, err := readQueue(a.queuePath()); err != nil || len(rows) != 0 {
		t.Fatal(len(rows), err)
	}
	if err := a.deleteSourcesFrom([]string{"-"}, strings.NewReader("https://example.com/page\n")); err == nil {
		t.Fatal("stdin deletion must not send a server removal")
	}
}

func TestCaptureLargeBacklogIsNotStranded(t *testing.T) {
	a := projectEntryApp(t)
	now := time.Now()
	for i := range 3 {
		payload := json.RawMessage(fmt.Sprintf(`{"id":%d,"query":"%s"}`, i, strings.Repeat("&", 6<<20)))
		src, err := captureToolSource("mcp__fixture__get_document", payload)
		if err != nil || len(src) > 7<<20 {
			t.Fatal(len(src), err)
		}
		if err := a.registerQueueSource(src, now); err != nil {
			t.Fatal(err)
		}
	}
	if queued, failed, err := readHookQueueCounts(a.queuePath()); err != nil || queued != 3 || failed != 0 {
		t.Fatal(queued, failed, err)
	}
	lock, snapshot, err := a.beginCapture(now)
	if err != nil || lock == nil || len(snapshot) != 2 || len(formatQueue(snapshot)) > maxCaptureSnapshotBytes {
		t.Fatal(lock, len(snapshot), err)
	}
	if _, rows, err := captureSnapshot(lock.Token, a.captureTmp(lock.Token)); err != nil || len(rows) != 2 {
		t.Fatal(len(rows), err)
	}
	rows, err := readQueue(a.queuePath())
	if err != nil || len(rows) != 3 || rows[0].Attempts != 1 || rows[1].Attempts != 1 || rows[2].Attempts != 0 {
		t.Fatal(len(rows), err)
	}
}
