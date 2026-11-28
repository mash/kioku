package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/cli"
	"github.com/mash/kioku/internal/server"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/prompts"
)

func TestCLIFullWorkflow(t *testing.T) {
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
	t.Run("local key, UTC boundary, and writeback", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "meeting notes.md")
		uri := (&url.URL{Scheme: "file", Path: path}).String()
		const body = "hello\n"
		const stamp = "updated_at: 2026-11-20T15:00:00Z"
		const key = "kioku:local/20261121-5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
		write := func(content string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(content), 0640); err != nil {
				t.Fatal(err)
			}
		}
		read := func() string {
			t.Helper()
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		write("---\n" + stamp + "\ntitle: Original\nextra: [one, two]\n---\n" + body)
		if out := must("put", uri); out != "accepted "+key+"\n" {
			t.Fatal(out)
		}
		first := read()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0640 || !strings.Contains(first, "source_url: "+key) || !strings.Contains(first, stamp+"\ntitle: Original\nextra: [one, two]") || !strings.HasSuffix(first, "---\n"+body) {
			t.Fatal(fi.Mode(), first)
		}
		if out := must("put", uri); out != "accepted "+key+"\n" {
			t.Fatal(out)
		}
		if out := must("status"); !strings.Contains(out, `"inbox_count": 2`) {
			t.Fatal(out)
		}
		write("---\n" + stamp + "\nsource_url: &original kioku:local\noriginal_marker: *original\n---\n" + body)
		if out := must("put", uri); out != "accepted "+key+"\n" || !strings.Contains(read(), "original_marker: kioku:local") {
			t.Fatal(out, read())
		}

		write(strings.Replace(first, "title: Original", "title: Revised", 1))
		if out := must("put", uri); out != "accepted "+key+"\n" {
			t.Fatal(out)
		}
		write(strings.Replace(first, body, "Hallo\n", 1))
		if out := must("put", uri); !strings.HasPrefix(out, "accepted kioku:local/20261121-") || out == "accepted "+key+"\n" {
			t.Fatal(out)
		}
		t.Run("delete local key without reading the HTTPS queue", func(t *testing.T) {
			queue := filepath.Join(config, ".kioku", "queue.tsv")
			const malformed = "invalid queue row\n"
			if err := os.WriteFile(queue, []byte(malformed), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Remove(queue); err != nil {
					t.Error(err)
				}
			})
			if out, err := run("delete", key); err != nil || out != "removed "+key+"\n" {
				t.Fatal(out, err)
			}
			if data, err := os.ReadFile(queue); err != nil || string(data) != malformed {
				t.Fatal(string(data), err)
			}
		})
		write(strings.Replace(first, "15:00:00Z", "14:59:59Z", 1))
		if out := must("put", uri); !strings.HasPrefix(out, "accepted kioku:local/20261120-") {
			t.Fatal(out)
		}
		write("hello\r\n")
		out := must("put", "--date", "2026-11-20", uri)
		written := read()
		if !strings.HasPrefix(out, "accepted kioku:local/20261120-") ||
			!strings.Contains(written, `updated_at: "2026-11-20T00:00:00+09:00"`) ||
			!strings.Contains(written, "\nsource_url: "+strings.TrimPrefix(out, "accepted ")) ||
			!strings.HasSuffix(written, "hello\r\n") || must("put", uri) != out {
			t.Fatal(out, written)
		}

		reject := func(want string, args ...string) {
			t.Helper()
			count := putCount.Load()
			out, err := run(append([]string{"put"}, args...)...)
			if err == nil || !strings.Contains(err.Error(), want) || out != "" || putCount.Load() != count {
				t.Fatal(args, out, err)
			}
		}
		reject("には updated_at がある", "--date", "2026-11-20", uri)
		reject("file:///abs/notes.md", path)
		write(body)
		reject("put: meeting notes.md に updated_at が無い。資料自身の日付（例: 会議の日）を --date 2026-11-20 で渡すか、frontmatter に updated_at を書く。ファイルの更新時刻や put した時刻で補わない", uri)
		reject("--date は YYYY-MM-DD", "--date", "2026-02-29", uri)
		write("---\n" + stamp + "\nsource_url: https://example.com/a\n---\n" + body)
		reject("通常のパスで put する: kioku put '"+path+"'", uri)
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

		write(body)
		change := func(w http.ResponseWriter, r *http.Request) {
			if err := os.WriteFile(path, []byte("edited during upload\n"), 0640); err != nil {
				t.Error(err)
			}
			handler.ServeHTTP(w, r)
		}
		afterPut.Store(&change)
		out, err = run("put", "--date", "2026-11-21", uri)
		afterPut.Store(nil)
		if out != "accepted "+key+"\n" || err == nil || !strings.Contains(err.Error(), "変わったので書き戻さなかった") || read() != "edited during upload\n" {
			t.Fatal(out, err, read())
		}
		if os.Geteuid() != 0 {
			write(body)
			readOnly := func(w http.ResponseWriter, r *http.Request) {
				if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
					t.Error(err)
				}
				handler.ServeHTTP(w, r)
			}
			afterPut.Store(&readOnly)
			out, err = run("put", "--date", "2026-11-21", uri)
			afterPut.Store(nil)
			if chmodErr := os.Chmod(filepath.Dir(path), 0700); chmodErr != nil {
				t.Fatal(chmodErr)
			}
			if out != "accepted "+key+"\n" || err == nil || !strings.Contains(err.Error(), "に書き戻せなかった") || read() != body {
				t.Fatal(out, err, read())
			}
		}

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
		out, err = run("put", "--date", "2026-11-21", uri)
		afterPut.Store(nil)
		if out != "" || err == nil || !strings.Contains(err.Error(), "受付を確かめられない") || read() != body {
			t.Fatal(out, err, read())
		}
		if out := must("put", "--date", "2026-11-21", uri); out != "accepted "+key+"\n" {
			t.Fatal(out)
		}

		write(body)
		closedOutput, err := os.CreateTemp(t.TempDir(), "output")
		if err != nil {
			t.Fatal(err)
		}
		if err := closedOutput.Close(); err != nil {
			t.Fatal(err)
		}
		err = cli.Run(ctx, []string{"-C", config, "--server", ts.URL, "put", "--date", "2026-11-21", uri}, closedOutput, &bytes.Buffer{})
		if err == nil || !strings.Contains(read(), "source_url: "+key) {
			t.Fatal("accepted put must write back even when stdout is closed", err, read())
		}

		const maxFile = 1 << 20
		header := "---\n" + stamp + "\nsource_url: kioku:local\n---\n"
		write(header + strings.Repeat("x", maxFile-len(header)-65))
		out = must("put", uri)
		if !strings.HasPrefix(out, "accepted kioku:local/20261121-") || len(read()) <= maxFile {
			t.Fatal("fixture must write back a file larger than 1 MiB", out)
		}
		if repeated, err := run("put", uri); err != nil || repeated != out {
			t.Fatal(out, repeated, err)
		}
		write(header + strings.Repeat("x", maxFile-len(header)+1))
		reject("exceeds 1 MiB", uri)
	})
	t.Run("local writeback uses the file's resolved parent", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("requires a non-writable directory")
		}
		lexical, actual := t.TempDir(), t.TempDir()
		child := filepath.Join(actual, "child")
		if err := os.Mkdir(child, 0700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(lexical, "link")
		if err := os.Symlink(child, link); err != nil {
			t.Fatal(err)
		}
		actualFile := filepath.Join(actual, "notes.md")
		if err := os.WriteFile(actualFile, []byte("notes\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lexical, 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(lexical, 0700) })
		uri := (&url.URL{Scheme: "file", Path: link + "/../notes.md"}).String()
		out, err := run("put", "--date", "2026-11-20", uri)
		data, readErr := os.ReadFile(actualFile)
		if err != nil || readErr != nil || !strings.HasPrefix(out, "accepted kioku:local/20261120-") || !strings.Contains(string(data), "source_url: "+strings.TrimSpace(strings.TrimPrefix(out, "accepted "))) {
			t.Fatal(out, err, string(data), readErr)
		}
	})
	t.Run("capture retry, status, and delete against SQLite", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_SESSION_ID", "session-1")
		p := t.TempDir()
		t.Setenv("PATH", p)
		if err := os.WriteFile(filepath.Join(p, "claude"), []byte("#!/bin/sh\nprintf 'backgrounded · 1a2b3c4d\n'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		const okURL = "https://example.com/captured"
		const failedURL = "https://example.com/login"
		const goneURL = "https://example.com/deleted"
		const abandonedURL = "https://example.com/abandoned"
		type status struct {
			Queue []struct {
				URL, State, Reason string
				Attempts           int
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
			must("capture", "run", "--min", "1", "--plugin-root", p)
			token, _ := readLock()
			return token
		}
		result := func(token, state, reason, u string) {
			t.Helper()
			must("capture", "result", "--capture-token", token, "--result", state, "--reason", reason, u)
		}
		must("put", okURL, failedURL, goneURL)
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
		if out := must("capture", "put", "--capture-token", token, path); out != "accepted capture.md\n" {
			t.Fatal(out)
		}
		result(token, "accepted", "受付済み", okURL)
		must("delete", goneURL)
		write(goneURL)
		if out, err := run("capture", "put", "--capture-token", token, path); err == nil || !strings.Contains(err.Error(), "delete 済み") || out != "" {
			t.Fatal(out, err)
		}
		must("put", goneURL)
		count := putCount.Load()
		if out, err := run("capture", "put", "--capture-token", token, path); err == nil || out != "" || putCount.Load() != count {
			t.Fatal("old snapshot must not submit after delete and requeue", out, err)
		}
		result(token, "unknown", "delete 済み", goneURL)
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

		for i, state := range []string{"unsaved", "unknown", "unsaved"} {
			if i != 0 {
				token = start()
			}
			result(token, state, "ログイン画面", failedURL)
			must("capture", "unlock", "--token", token)
			st = readStatus()
			want := "queued"
			if i == 2 {
				want = "failed"
			}
			if st.Capture != nil || len(st.Queue) != 1 || st.Queue[0].State != want || st.Queue[0].Attempts != i+1 || st.Queue[0].Reason != "ログイン画面" {
				t.Fatalf("%+v", st)
			}
		}
		must("put", failedURL)
		st = readStatus()
		if len(st.Queue) != 1 || st.Queue[0].State != "queued" || st.Queue[0].Attempts != 0 || st.Queue[0].Reason != "" {
			t.Fatalf("%+v", st)
		}
		must("delete", failedURL)
		must("put", abandonedURL)
		for range 3 {
			start()
			_, lock := readLock()
			lock["started_at"] = time.Now().Add(-31 * time.Minute)
			b, _ := json.Marshal(lock)
			if err := os.WriteFile(lockPath, b, 0600); err != nil {
				t.Fatal(err)
			}
		}
		must("capture", "run", "--min", "1", "--plugin-root", p)
		st = readStatus()
		if st.Capture != nil || len(st.Queue) != 1 || st.Queue[0].State != "failed" || st.Queue[0].Reason != "capture が 3 回とも結果を返さなかった" {
			t.Fatalf("%+v", st)
		}

		const blob = "https://github.com/owner/repo/blob/main/source.md"
		const raw = "https://raw.githubusercontent.com/owner/repo/main/source.md"
		must("put", blob, raw)
		token = start()
		must("delete", blob)
		dir, err = filepath.EvalSymlinks(filepath.Join(config, ".kioku", "capture-tmp", token))
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, "capture.md")
		write(raw)
		if out, err := run("capture", "put", "--capture-token", token, path); err != nil || out != "accepted capture.md\n" {
			t.Fatal("deleting the blob URL must not prevent capture of the separately queued raw URL", out, err)
		}
		result(token, "accepted", "受付済み", raw)
		must("capture", "unlock", "--token", token)
	})
	must("logout")
	if _, err = run("whoami"); err == nil {
		t.Fatal("logout did not revoke session")
	}
}
