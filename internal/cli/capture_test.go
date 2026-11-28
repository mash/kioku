package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

// fakeCaptureClaude installs a shell fixture; it never invokes Claude Code.
func fakeCaptureClaude(t *testing.T, output string, status int) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
if [ "$1" = rm ]; then
 printf '%s\n' "$2" >> "$CAPTURE_TEST_LOG.rm"
 exit 1
fi
printf '%s\000' "$@" > "$CAPTURE_TEST_LOG.args"
pwd > "$CAPTURE_TEST_LOG.cwd"
printf '%s' "${CLAUDE_CODE_SESSION_ID-unset}" > "$CAPTURE_TEST_LOG.sid"
IFS= read -r line && exit 9
printf '%s' "$CAPTURE_TEST_OUTPUT"
if [ "$CAPTURE_TEST_STATUS" != 0 ]; then printf 'Workspace not trusted\n' >&2; fi
exit "$CAPTURE_TEST_STATUS"
`
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CAPTURE_TEST_LOG", log)
	t.Setenv("CAPTURE_TEST_OUTPUT", output)
	t.Setenv("CAPTURE_TEST_STATUS", strconv.Itoa(status))
	return log
}

func BenchmarkBeginCapture(b *testing.B) {
	a := &App{Dir: b.TempDir(), Config: Config{Namespace: "org/p"}, Out: io.Discard}
	if err := a.ensureProject(); err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC()
	rows := make([]queueRow, 512)
	for i := range rows {
		rows[i] = queueRow{Date: "2026-11-28", Source: fmt.Sprintf("https://example.com/%d", i), State: queueQueued}
	}
	data := formatQueue(rows)
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		if err := os.WriteFile(a.queuePath(), data, 0600); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		lock, _, err := a.beginCapture(now)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := a.discardCapture(lock.Token); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

func TestCaptureRun(t *testing.T) {
	setup := func(t *testing.T, n int) *App {
		t.Helper()
		sessionTestEnv(t)
		t.Setenv("TMPDIR", t.TempDir())
		a := projectEntryApp(t)
		a.Config.Namespace = "org/a"
		a.Out = &bytes.Buffer{}
		if err := ensureDirectory(a.metadataDir()); err != nil {
			t.Fatal(err)
		}
		var rows []queueRow
		for i := range n {
			rows = append(rows, queueRow{"2026-11-18", fmt.Sprintf("https://x.example/%d", i), 0, queueQueued, ""})
		}
		if err := os.WriteFile(filepath.Join(a.metadataDir(), "queue.tsv"), formatQueue(rows), 0600); err != nil {
			t.Fatal(err)
		}
		return a
	}
	t.Run("I14_missing_id", func(t *testing.T) {
		a := setup(t, 5)
		log := fakeCaptureClaude(t, "started without id\n", 0)
		last := filepath.Join(a.metadataDir(), "capture.last")
		if err := os.WriteFile(last, []byte("aaaaaaaa\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.captureRun(t.TempDir(), time.Now()); err != nil {
			t.Fatal(err)
		}
		lock, err := readCaptureLock(filepath.Join(a.metadataDir(), "capture.lock"))
		if err != nil || lock.BgID != "" {
			t.Fatal(lock, err)
		}
		if got := a.Out.(*bytes.Buffer).String(); got != "kioku-capture を背景で起動した（org/a、5 件、id 不明）。capture.log を確認する。\n" {
			t.Fatal(got)
		}
		if _, err := os.Stat(log + ".rm"); !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if _, err := os.Stat(last); err != nil {
			t.Fatal(err)
		}
	})
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("E35_launch_failure/missing=%v", missing), func(t *testing.T) {
			a := setup(t, 5)
			fakeCaptureClaude(t, "", 1)
			if missing {
				t.Setenv("PATH", t.TempDir())
			}
			queue := filepath.Join(a.metadataDir(), "queue.tsv")
			before, _ := os.ReadFile(queue)
			err := a.captureRun(t.TempDir(), time.Now())
			want := "capture run: claude --bg failed: Workspace not trusted"
			if missing {
				want = "capture run: claude not found"
			}
			if err == nil || err.Error() != want || a.Out.(*bytes.Buffer).Len() != 0 {
				t.Fatal(err, a.Out)
			}
			if _, err := os.Stat(filepath.Join(a.metadataDir(), "capture.lock")); !os.IsNotExist(err) {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(queue)
			if !bytes.Equal(before, after) {
				t.Fatal("launch failure removed queue")
			}
		})
	}
	t.Run("E36_exclusive_stale_replacement", func(t *testing.T) {
		a := setup(t, 5)
		now := time.Now().UTC()
		path := filepath.Join(a.metadataDir(), "capture.lock")
		writeHookFixture(t, path, captureLock{PID: 123, StartedAt: now.Add(-time.Hour), BgID: "aaaaaaaa"})
		errs := make(chan error, 8)
		for range 8 {
			go func() { _, err := acquireCaptureLock(path, now); errs <- err }()
		}
		successes := 0
		for range 8 {
			err := <-errs
			if err == nil {
				successes++
			} else if !errors.Is(err, errCaptureBusy) {
				t.Fatal(err)
			}
		}
		if successes != 1 {
			t.Fatal(successes)
		}
	})
	t.Run("R2_fetch_confined_https", func(t *testing.T) {
		a := setup(t, 1)
		lock, err := acquireCaptureLock(filepath.Join(a.metadataDir(), "capture.lock"), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		tmp, err := a.prepareCaptureTmp(lock.Token)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Error(r.Method)
			}
			switch r.URL.Path {
			case "/chain":
				left, _ := strconv.Atoi(r.URL.Query().Get("left"))
				if left > 0 {
					http.Redirect(w, r, "/chain?left="+strconv.Itoa(left-1), http.StatusFound)
				} else {
					fmt.Fprint(w, "source body")
				}
			case "/foreign":
				http.Redirect(w, r, "https://example.invalid/", http.StatusFound)
			case "/downgrade":
				http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
			case "/redirect":
				http.Redirect(w, r, "https://"+r.Host+"/ok", http.StatusFound)
			case "/large":
				w.Header().Set("Content-Length", strconv.Itoa(maxFetchBytes+1))
			case "/chunked":
				w.(http.Flusher).Flush()
				_, _ = io.CopyN(w, strings.NewReader(strings.Repeat("x", maxFetchBytes+1)), maxFetchBytes+1)
			default:
				fmt.Fprint(w, "source body")
			}
		}))
		defer server.Close()
		var snapshot []queueRow
		for _, path := range []string{"", "/ok", "/redirect", "/downgrade", "/foreign", "/large", "/chunked", "/chain?left=9", "/chain?left=10"} {
			snapshot = append(snapshot, queueRow{"2026-11-18", server.URL + path, 0, queueQueued, ""})
		}
		if err := a.saveCaptureSnapshot(lock.Token, snapshot); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, path, out string
			ok              bool
		}{
			{"redirect_nine", "/chain?left=9", "nine", true}, {"redirect_ten", "/chain?left=10", "ten", false}, {"https", "/ok", "ok", true}, {"foreign_redirect", "/foreign", "foreign", false}, {"not_in_snapshot", "/secret", "secret", false}, {"https_redirect", "/redirect", "redirect", true}, {"downgrade", "/downgrade", "bad", false}, {"size_header", "/large", "large", false}, {"size_chunked", "/chunked", "chunked", false}, {"escape", "/ok", "../outside", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				err := fetchURL(context.Background(), server.Client().Transport, server.URL+tc.path, tmp, tc.out)
				if (err == nil) != tc.ok {
					t.Fatal(err)
				}
				if tc.ok {
					info, err := os.Stat(filepath.Join(tmp, tc.out))
					if err != nil || info.Mode().Perm() != 0600 {
						t.Fatal(info, err)
					}
				}
			})
		}
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(tmp, "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Dir(outside), filepath.Join(tmp, "dirlink")); err != nil {
			t.Fatal(err)
		}
		for _, out := range []string{"link", "dirlink/outside", "ok"} {
			if err := fetchURL(context.Background(), server.Client().Transport, server.URL, tmp, out); err == nil {
				t.Fatal("accepted existing file or symlink", out)
			}
		}
		if data, _ := os.ReadFile(outside); string(data) != "untouched" {
			t.Fatal(string(data))
		}

		if err := a.capturePut([]string{"--capture-token", lock.Token, outside}); err == nil {
			t.Fatal("capture put accepted outside file")
		}
		for _, path := range []string{outside, filepath.Join(tmp, "link"), filepath.Join(tmp, "dirlink/outside")} {
			if _, err := readCaptureSource(tmp, path); err == nil {
				t.Fatal("capture put read outside or symlink", path)
			}
		}
		if data, err := readCaptureSource(tmp, filepath.Join(tmp, "ok")); err != nil || string(data) != "source body" {
			t.Fatal(string(data), err)
		}

		if err := fetchCommand(context.Background(), []string{server.URL, "--capture-token", lock.Token, "--out", "x", "--dir", filepath.Dir(outside)}); err == nil {
			t.Fatal("accepted foreign --dir")
		}
		if err := fetchURL(context.Background(), nil, "http://example.invalid", tmp, "http"); err == nil {
			t.Fatal("accepted http")
		}
	})
	t.Run("lingering_supervisor", func(t *testing.T) {
		a := setup(t, 1)
		log := fakeCaptureClaude(t, "", 0)
		oldDelay := captureWaitDelay
		captureWaitDelay = 100 * time.Millisecond
		t.Cleanup(func() {
			captureWaitDelay = oldDelay
			data, err := os.ReadFile(log + ".pid")
			if err != nil {
				t.Error(err)
				return
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Error(err)
				return
			}
			process, err := os.FindProcess(pid)
			if err == nil {
				_ = process.Kill()
			}
		})
		script := "#!/bin/sh\n/bin/sleep 7 &\nprintf '%s\\n' \"$!\" > \"$CAPTURE_TEST_LOG.pid\"\nprintf 'backgrounded · 1a2b3c4d\\n'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(filepath.Dir(log), "claude"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := a.captureRun(t.TempDir(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatal("blocked on supervisor", elapsed)
		}
		lock, _ := readCaptureLock(filepath.Join(a.metadataDir(), "capture.lock"))
		if lock == nil || lock.BgID != "1a2b3c4d" {
			t.Fatal(lock)
		}
	})
	t.Run("R4_github_blob_fetch", func(t *testing.T) {
		a := setup(t, 1)
		lock, err := acquireCaptureLock(a.captureLockPath(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		tmp, err := a.prepareCaptureTmp(lock.Token)
		if err != nil {
			t.Fatal(err)
		}
		original := "https://github.com/owner/repo/blob/abc123/docs/source.md"
		if err := a.saveCaptureSnapshot(lock.Token, []queueRow{{"2026-11-18", original, 0, queueQueued, ""}}); err != nil {
			t.Fatal(err)
		}
		requests := 0
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if r.Host != "raw.githubusercontent.com" || r.URL.Path != "/owner/repo/abc123/docs/source.md" {
				t.Error(r.Host, r.URL)
			}
			fmt.Fprint(w, "raw source")
		}))
		defer server.Close()
		transport := server.Client().Transport.(*http.Transport).Clone()
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		transport.TLSClientConfig.ServerName = "example.com"
		defer transport.CloseIdleConnections()
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "raw.githubusercontent.com:443" {
				t.Errorf("unexpected destination %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		if err := fetchURL(context.Background(), transport, original, tmp, "source"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(tmp, "source"))
		if err != nil || string(data) != "raw source" || requests != 1 {
			t.Fatal(string(data), requests, err)
		}
		if err := fetchURL(context.Background(), transport, "https://raw.githubusercontent.com/owner/repo/abc123/docs/source.md", tmp, "direct"); err == nil || requests != 1 {
			t.Fatal("accepted URL outside snapshot", err)
		}
	})
	t.Run("R8_capture_source_and_extensions", func(t *testing.T) {
		a := setup(t, 0)
		now := time.Now()
		original := "https://github.com/owner/repo/blob/main/docs/source.md"
		raw := "https://raw.githubusercontent.com/owner/repo/main/docs/source.md"
		if err := a.registerQueueSource(original, now); err != nil {
			t.Fatal(err)
		}
		lock, _, err := a.beginCapture(now)
		if err != nil {
			t.Fatal(err)
		}
		tmp, err := resolveCaptureDir(lock.Token, a.captureTmp(lock.Token))
		if err != nil {
			t.Fatal(err)
		}
		writes := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writes++
			var request model.PutRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			doc, err := source.FileWithProvenance(request.File.Name, request.File.Content)
			if err != nil || doc.URL != original || !strings.HasSuffix(request.File.Content, "Evidence") {
				t.Error(doc, err, request.File.Content)
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, "{}")
		}))
		defer server.Close()
		a.Config.Server, a.Client, a.ctx = server.URL, server.Client(), context.Background()
		file := filepath.Join(tmp, "source.md")
		for _, u := range []string{original, raw} {
			if err := os.WriteFile(file, []byte(SourceContentForTest(u, "Evidence")), 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.capturePut([]string{"--capture-token", lock.Token, "--source", original, file}); (err == nil) != (u == original) {
				t.Fatal(err, u)
			}
		}
		if writes != 1 {
			t.Fatal(writes)
		}
		for _, tc := range []struct{ path, mime, ext string }{
			{"/a.PDF?download=1", "text/plain", ".pdf"},
			{"/a.png", "", ".png"}, {"/a.jpg", "", ".jpg"}, {"/a.jpeg", "", ".jpeg"},
			{"/a.gif", "", ".gif"}, {"/a.webp", "", ".webp"}, {"/a.md", "", ".md"},
			{"/a.txt", "", ".txt"}, {"/a.html", "", ".html"},
			{"/download", "application/pdf", ".pdf"}, {"/download", "image/png", ".png"},
			{"/download", "image/jpeg", ".jpg"}, {"/download", "image/gif", ".gif"},
			{"/download", "image/webp", ".webp"}, {"/download", "text/markdown", ".md"},
			{"/download", "text/plain; charset=utf-8", ".txt"},
			{"/download", "text/html", ".html"}, {"/download", "application/octet-stream", ".raw"},
		} {
			t.Run(tc.path+tc.mime, func(t *testing.T) {
				if got := captureFetchExtension("https://example.com"+tc.path, tc.mime); got != tc.ext {
					t.Fatal(got, tc.ext)
				}
			})
		}
		fetchServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/pdf")
			fmt.Fprint(w, "%PDF-test")
		}))
		defer fetchServer.Close()
		if err := a.saveCaptureSnapshot(lock.Token, []queueRow{{"2026-11-18", fetchServer.URL, 0, queueQueued, ""}}); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := fetchURL(context.Background(), fetchServer.Client().Transport, fetchServer.URL, tmp, "download.raw", &output); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(tmp, "download.pdf")
		data, err := os.ReadFile(want)
		if err != nil || string(data) != "%PDF-test" || !strings.HasPrefix(output.String(), want+"\nfetched_at: ") {
			t.Fatal(err, output.String(), string(data))
		}
		if _, err := os.Stat(filepath.Join(tmp, "download.raw")); !os.IsNotExist(err) {
			t.Fatal(err)
		}
		commands := captureCommands("'/bin/kioku'", "'/bin/kioku' -C '/p' -n org/a", tmp, lock.Token, []queueRow{{"2026-11-18", original, 0, queueQueued, ""}})
		if !strings.Contains(commands, "source-001.input.md") || !strings.Contains(commands, "source-001.md") {
			t.Fatal(commands)
		}
	})
	t.Run("R9_fetch_timestamps", func(t *testing.T) {
		a := setup(t, 1)
		lock, err := acquireCaptureLock(a.captureLockPath(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		tmp, err := a.prepareCaptureTmp(lock.Token)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/valid":
				w.Header().Set("Last-Modified", "Wed, 18 Nov 2026 03:04:05 GMT")
			case "/invalid":
				w.Header().Set("Last-Modified", "not a timestamp")
			}
			fmt.Fprint(w, "source")
		}))
		defer server.Close()
		var rows []queueRow
		for _, name := range []string{"valid", "missing", "invalid"} {
			rows = append(rows, queueRow{"2026-11-18", server.URL + "/" + name, 0, queueQueued, ""})
		}
		if err := a.saveCaptureSnapshot(lock.Token, rows); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			t.Run(filepath.Base(row.Source), func(t *testing.T) {
				var output bytes.Buffer
				name := filepath.Base(row.Source) + ".txt"
				before := time.Now().UTC().Truncate(time.Second)
				if err := fetchURL(context.Background(), server.Client().Transport, row.Source, tmp, name, &output); err != nil {
					t.Fatal(err)
				}
				after := time.Now().UTC()
				lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
				wantLines := 2
				if name == "valid.txt" {
					wantLines = 3
				}
				if len(lines) != wantLines || lines[0] != filepath.Join(tmp, name) || !strings.HasPrefix(lines[1], "fetched_at: ") {
					t.Fatal(output.String())
				}
				stamp := strings.TrimPrefix(lines[1], "fetched_at: ")
				fetched, err := time.Parse(time.RFC3339, stamp)
				if err != nil || !strings.HasSuffix(stamp, "Z") || fetched.Before(before) || fetched.After(after) {
					t.Fatal(stamp, err, before, after)
				}
				if wantLines == 3 && lines[2] != "last_modified: 2026-11-18T03:04:05Z" {
					t.Fatal(output.String())
				}
			})
		}
	})
}
