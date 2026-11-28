package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/cli"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/server"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/prompts"
)

func TestCLIFullWorkflow(t *testing.T) {
	// Build before HOME moves to a temp dir, so go keeps the real module cache.
	binary := filepath.Join(t.TempDir(), "kioku")
	build := exec.Command("go", "build", "-o", binary, "./cmd/kioku")
	build.Dir = filepath.Join("..", "..")
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %s: %v", data, err)
	}
	data := t.TempDir()
	stateStore, err := store.OpenDatabase(filepath.Join(data, "catalog.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	key, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := prompts.Load(prompts.Variables{ClientVersion: server.MinimumClientVersion, MaxOutputBytes: 128 << 20}, server.MCPToolNames())
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(server.Config{Prompts: rendered, GenerateDebounce: time.Hour, BaseURL: "http://127.0.0.1"}, stateStore, local.Store{Dir: filepath.Join(data, "objects")}, key)
	var putCount atomic.Int32
	var afterPut atomic.Pointer[func(http.ResponseWriter, *http.Request)]
	handler := s.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/put") {
			putCount.Add(1)
			if hook := afterPut.Load(); hook != nil {
				(*hook)(w, r)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	s.Config.BaseURL = ts.URL
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.RunWorker(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		ts.Close()
		stateStore.Close()
	}()
	config := t.TempDir()
	run := func(args ...string) (string, error) {
		t.Helper()
		var out, stderr bytes.Buffer
		all := append([]string{"-C", config, "--server", ts.URL}, args...)
		err := cli.Run(ctx, all, &out, &stderr)
		return out.String(), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	// Authentication is seeded only in this test, never through a server route.
	token := auth.Token()
	if err = stateStore.Write(t.Context(), func(q *store.Queries) error {
		if e := q.PutUser(store.UserRow{ID: "test-user", Email: "alice@example.com", Data: store.UserData{Name: "Alice"}}); e != nil {
			return e
		}
		if e := q.PutIdentity("test-user", "test", "alice"); e != nil {
			return e
		}
		raw, e := json.Marshal(store.IdentitySessionData{})
		if e != nil {
			return e
		}
		return q.PutSession(store.SessionRow{ID: auth.Hash(token), Kind: "user", Status: "active", UserID: "test-user", Expires: time.Now().Add(time.Hour).UnixMilli(), Data: raw})
	}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".config", "kioku", "auth")
	if err := os.MkdirAll(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	initial, _ := json.Marshal(map[string]string{"server": ts.URL, "token": token})
	if err = os.WriteFile(filepath.Join(authDir, auth.Hash(ts.URL)+".json"), initial, 0600); err != nil {
		t.Fatal(err)
	}
	must("whoami")
	must("org", "create", "test-org")
	must("-n", "test-org/test-project", "project", "create")
	must("project", "share", "list")
	must("project", "share", "invite", "bob@example.com")
	b, err := os.ReadFile(filepath.Join(config, ".kioku", "config"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg cli.Config
	if err = json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "notes.md")
	if err = os.WriteFile(source, []byte(cli.SourceContentForTest("https://example.com/notes", "# Architecture\nAll application code is Go. Objects live in S3.")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := run("put", "--source-url", "https://example.com/override", source); err == nil {
		t.Fatal("accepted removed source-url flag")
	}
	must("put", source)
	if out := must("status"); !strings.Contains(out, `"inbox_count": 1`) {
		t.Fatal(out)
	}
	if out := must("put", source); !strings.Contains(out, "accepted notes.md") {
		t.Fatal(out)
	}
	t.Run("local source uses mtime without changing the original", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "meeting notes.md")
		uri := (&url.URL{Scheme: "file", Path: path}).String()
		const body = "hello\n"
		const key = "kioku:local/5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
		mtime := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
		write := func(content string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(content), 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
		unchanged := func(content string) {
			t.Helper()
			b, err := os.ReadFile(path)
			if err != nil || string(b) != content {
				t.Fatal(string(b), err)
			}
			fi, err := os.Stat(path)
			if err != nil || fi.Mode().Perm() != 0640 || !fi.ModTime().Equal(mtime) {
				t.Fatal(fi, err)
			}
		}
		first := "---\nupdated_at: [invalid]\ntitle: Original\nextra: [one, two]\n---\n" + body
		write(first)
		inspect := func(w http.ResponseWriter, r *http.Request) {
			b, err := io.ReadAll(r.Body)
			var req model.PutRequest
			if err != nil || json.Unmarshal(b, &req) != nil || req.File == nil {
				t.Error("invalid put request", err)
			} else if got := req.File.Content; !strings.Contains(got, "source_url: kioku:local") || !strings.Contains(got, mtime.Format(time.RFC3339)) || !strings.HasSuffix(got, "---\n"+body) {
				t.Error(got)
			}
			r.Body = io.NopCloser(bytes.NewReader(b))
			handler.ServeHTTP(w, r)
		}
		afterPut.Store(&inspect)
		count := putCount.Load()
		for range 2 {
			if out := must("put", uri); out != "accepted "+key+"\n" {
				t.Fatal(out)
			}
			unchanged(first)
		}
		afterPut.Store(nil)
		if out := must("status"); !strings.Contains(out, `"inbox_count": 2`) || putCount.Load() != count+2 {
			t.Fatal(out)
		}
		mtime = mtime.Add(25 * time.Hour)
		write(strings.Replace(first, "Original", "Revised", 1))
		if out := must("put", uri); out != "accepted "+key+"\n" {
			t.Fatal(out)
		}
		write("Hallo\n")
		if out := must("put", uri); !strings.HasPrefix(out, "accepted kioku:local/") || out == "accepted "+key+"\n" {
			t.Fatal(out)
		}
		unchanged("Hallo\n")

		// A Markdown horizontal rule is not frontmatter without a closing delimiter.
		const plain = "---\n\n# Notes\n\nPlain Markdown\n"
		write(plain)
		if out := must("put", uri); out != "accepted kioku:local/3022fe26ef68ec5b83ada5f05ee8141c44fa4a283a67a8e2c7cfcd0b009f5bb5\n" {
			t.Fatal(out)
		}
		unchanged(plain)

		queue := filepath.Join(config, ".kioku", "queue.tsv")
		if err := os.WriteFile(queue, []byte("invalid queue row\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if out := must("delete", key); out != "removed "+key+"\n" {
			t.Fatal(out)
		}
		if b, err := os.ReadFile(queue); err != nil || string(b) != "invalid queue row\n" {
			t.Fatal(string(b), err)
		}
		if err := os.Remove(queue); err != nil {
			t.Fatal(err)
		}

		reject := func(want string, args ...string) {
			t.Helper()
			count := putCount.Load()
			out, err := run(append([]string{"put"}, args...)...)
			if err == nil || !strings.Contains(err.Error(), want) || out != "" || putCount.Load() != count {
				t.Fatal(args, out, err)
			}
		}
		write("---\nsource_url: https://example.com/a\n---\n" + body)
		reject("通常のパスで put", uri)
		for _, u := range []string{"ftp://example.com/x", key, "null"} {
			write("---\nsource_url: " + u + "\n---\n" + body)
			reject("省くか kioku:local", uri)
		}
		write("---\nsource_url: " + key + "\n---\n" + body)
		reject("delete", source, path)
		write("---\nsource_url: kioku:local\n---\n" + body)
		reject("file:///abs/notes.md", source, path)
		reject("ホストとユーザー情報", "file://host/a.md")
		reject("クエリとフラグメント", uri+"?#")
		reject("解釈できない", "file:a.md")
		reject("解釈できない", "file:///%zz")
		reject("通常ファイル 1 つだけ", (&url.URL{Scheme: "file", Path: filepath.Dir(path)}).String())
		link := filepath.Join(filepath.Dir(path), "link.md")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		reject("シンボリックリンク", (&url.URL{Scheme: "file", Path: link}).String())
		reject("usage:", uri, path)
		reject("usage:", uri, uri)
		reject("usage:", source, "--date")

		write(body)
		lost := func(w http.ResponseWriter, r *http.Request) {
			handler.ServeHTTP(httptest.NewRecorder(), r)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		}
		afterPut.Store(&lost)
		out, err := run("put", uri)
		afterPut.Store(nil)
		if out != "" || err == nil || !strings.Contains(err.Error(), "本文を変えずに同じコマンド") {
			t.Fatal(out, err)
		}
		unchanged(body)
		if out := must("put", uri); out != "accepted "+key+"\n" {
			t.Fatal(out)
		}
		unchanged(body)

		t.Run("command exits one on rejected local puts and incomplete delete keys", func(t *testing.T) {
			runRejected := func(want string, args ...string) {
				t.Helper()
				var out, stderr bytes.Buffer
				cmd := exec.CommandContext(ctx, binary, append([]string{"-C", config, "--server", ts.URL}, args...)...)
				cmd.Stdout, cmd.Stderr = &out, &stderr
				err := cmd.Run()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 || out.Len() != 0 || !strings.Contains(stderr.String(), want) {
					t.Fatalf("%v: stdout=%q stderr=%q error=%v", args, out.String(), stderr.String(), err)
				}
			}
			for _, code := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
				rejectPut := func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(code)
					_, _ = io.WriteString(w, `{"error":"fixture rejection"}`)
				}
				afterPut.Store(&rejectPut)
				count := putCount.Load()
				runRejected("server error: fixture rejection", "put", uri)
				afterPut.Store(nil)
				if putCount.Load() != count+1 {
					t.Fatal("expected one put request")
				}
				unchanged(body)
			}
			count := putCount.Load()
			runRejected("ローカルキー", "delete", "kioku:local")
			if putCount.Load() != count {
				t.Fatal("sent remove for an incomplete key")
			}
		})

		write(strings.Repeat("x", (1<<20)+1))
		reject("exceeds 1 MiB", uri)

		write(cli.SourceContentForTest("https://127.0.0.1:8443/login", "Authenticated source"))
		if out := must("put", path); out != "accepted meeting notes.md\n" {
			t.Fatal(out)
		}
	})
	t.Run("capture retry, status, and delete against SQLite", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_SESSION_ID", "session-1")
		p := t.TempDir()
		t.Setenv("PATH", p)
		t.Setenv("CLAUDE_PLUGIN_ROOT", p)
		if err := os.WriteFile(filepath.Join(p, "claude"), []byte("#!/bin/sh\nprintf 'backgrounded · 1a2b3c4d\n'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		const okURL = "https://example.com/captured"
		const failedURL = "https://example.com/login"
		const goneURL = "https://example.com/deleted"
		const abandonedURL = "https://example.com/abandoned"
		type status struct {
			Queue []struct {
				Source, State, Reason string
				Attempts              int
			}
			Capture *struct {
				ID        string
				StartedAt time.Time `json:"started_at"`
			}
			Inbox []struct {
				SourceURL string `json:"source_url"`
				Remove    bool
			}
		}
		readStatus := func() status {
			t.Helper()
			out := must("status")
			var got status
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(out, err)
			}
			return got
		}
		lockPath := filepath.Join(config, ".kioku", "capture.lock")
		readLock := func() (string, map[string]any) {
			t.Helper()
			b, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			var lock map[string]any
			if err = json.Unmarshal(b, &lock); err != nil {
				t.Fatal(err)
			}
			return lock["token"].(string), lock
		}
		start := func() string {
			t.Helper()
			must("capture", "run")
			token, _ := readLock()
			return token
		}
		result := func(token, state, reason, u string) {
			t.Helper()
			args := []string{"capture", "result", "--capture-token", token, "--result", state}
			if reason != "" {
				args = append(args, "--reason", reason)
			}
			must(append(args, u)...)
		}
		enqueue := func(urls ...string) {
			t.Helper()
			q := filepath.Join(config, ".kioku", "queue.tsv")
			f, err := os.OpenFile(q, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			for _, u := range urls {
				if _, err := fmt.Fprintf(f, "%s\t%s\t0\tqueued\t\n", time.Now().Format(time.DateOnly), u); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}
		enqueue(okURL, failedURL, goneURL)
		token := start()
		st := readStatus()
		if st.Capture == nil || st.Capture.ID != "1a2b3c4d" || st.Capture.StartedAt.IsZero() || len(st.Queue) != 3 || st.Queue[0].Attempts != 1 {
			t.Fatalf("%+v", st)
		}
		dir, err := filepath.EvalSymlinks(filepath.Join(config, ".kioku", "capture-tmp", token))
		if err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(dir, "nested")
		if err := os.Mkdir(nested, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(nested, "capture.md")
		write := func(u string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(cli.SourceContentForTest(u, "Captured")), 0600); err != nil {
				t.Fatal(err)
			}
		}
		write(okURL)
		if out := must("capture", "put", "--capture-token", token, "--source", okURL, path); out != "accepted "+okURL+"\n" {
			t.Fatal(out)
		}
		result(token, "accepted", "", okURL)
		must("delete", goneURL)
		write(goneURL)
		if out, err := run("capture", "put", "--capture-token", token, "--source", goneURL, path); err == nil || !strings.Contains(err.Error(), "delete 済み") || out != "" {
			t.Fatal(out, err)
		}
		enqueue(goneURL)
		count := putCount.Load()
		if out, err := run("capture", "put", "--capture-token", token, "--source", goneURL, path); err == nil || out != "" || putCount.Load() != count {
			t.Fatal("old snapshot must not submit after delete and requeue", out, err)
		}
		result(token, "skipped", "delete 済み", goneURL)
		for _, u := range []string{okURL, goneURL} {
			found := false
			for _, row := range readStatus().Inbox {
				if row.SourceURL == u {
					found = row.Remove == (u == goneURL)
				}
			}
			if !found {
				t.Fatal(u, readStatus())
			}
		}
		must("delete", goneURL)
		must("delete", okURL)
		for _, row := range readStatus().Inbox {
			if row.SourceURL == okURL && !row.Remove {
				t.Fatal("remove did not replace the accepted capture", row)
			}
		}

		for i, state := range []string{"unsaved", "unsaved"} {
			if i != 0 {
				token = start()
			}
			result(token, state, "ログイン画面", failedURL)
			st = readStatus()
			want := "queued"
			if i == 1 {
				want = "failed"
			}
			if st.Capture != nil || len(st.Queue) != 1 || st.Queue[0].State != want || st.Queue[0].Attempts != i+1 || st.Queue[0].Reason != "ログイン画面" {
				t.Fatalf("%+v", st)
			}
		}
		must("delete", failedURL)
		enqueue(failedURL)
		st = readStatus()
		if len(st.Queue) != 1 || st.Queue[0].State != "queued" || st.Queue[0].Attempts != 0 || st.Queue[0].Reason != "" {
			t.Fatalf("%+v", st)
		}
		must("delete", failedURL)
		enqueue(abandonedURL)
		for range 2 {
			start()
			_, lock := readLock()
			lock["started_at"] = time.Now().Add(-31 * time.Minute)
			b, _ := json.Marshal(lock)
			if err := os.WriteFile(lockPath, b, 0600); err != nil {
				t.Fatal(err)
			}
		}
		must("capture", "run")
		st = readStatus()
		if st.Capture != nil || len(st.Queue) != 1 || st.Queue[0].State != "failed" || st.Queue[0].Reason != "capture が 2 回とも結果を返さなかった" {
			t.Fatalf("%+v", st)
		}

		const blob = "https://github.com/owner/repo/blob/main/source.md"
		const raw = "https://raw.githubusercontent.com/owner/repo/main/source.md"
		enqueue(blob, raw)
		token = start()
		must("delete", blob)
		dir, err = filepath.EvalSymlinks(filepath.Join(config, ".kioku", "capture-tmp", token))
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, "capture.md")
		write(raw)
		if out, err := run("capture", "put", "--capture-token", token, "--source", raw, path); err != nil || out != "accepted "+raw+"\n" {
			t.Fatal("deleting the blob URL must not prevent capture of the separately queued raw URL", out, err)
		}
		result(token, "accepted", "", raw)
		result(token, "skipped", "delete 済み", blob)
	})
	must("logout")
	if _, err = run("whoami"); err == nil {
		t.Fatal("logout did not revoke session")
	}
}
