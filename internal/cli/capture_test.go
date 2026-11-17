package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
			rows = append(rows, queueRow{"2026-11-18", fmt.Sprintf("https://x.example/%d", i), "読んだ"})
		}
		if err := os.WriteFile(filepath.Join(a.metadataDir(), "queue.tsv"), formatQueue(rows), 0600); err != nil {
			t.Fatal(err)
		}
		return a
	}
	t.Run("E35_threshold_and_launch", func(t *testing.T) {
		a := setup(t, 3)
		a.Config.Server = "https://other.example"
		log := fakeCaptureClaude(t, "backgrounded · 1a2b3c4d\n", 0)
		now := time.Now().UTC()
		cwd := t.TempDir()
		t.Chdir(cwd)
		queue := filepath.Join(a.metadataDir(), "queue.tsv")
		before, _ := os.ReadFile(queue)
		if err := a.capture([]string{"run"}, now); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(a.Out.(*bytes.Buffer).String(), "queue below minimum") {
			t.Fatal(a.Out)
		}
		if _, err := os.Stat(log + ".args"); !os.IsNotExist(err) {
			t.Fatal("below threshold launched", err)
		}
		a.Out.(*bytes.Buffer).Reset()
		if err := a.capture([]string{"run", "--min", "1"}, now); err != nil {
			t.Fatal(err)
		}
		if got := a.Out.(*bytes.Buffer).String(); got != "kioku-capture を背景で起動した（org/a、3 件、id 1a2b3c4d）。claude logs 1a2b3c4d で見る。\n" {
			t.Fatal(got)
		}
		if a.Err.(*bytes.Buffer).Len() != 0 {
			t.Fatal(a.Err)
		}
		lockPath := filepath.Join(a.metadataDir(), "capture.lock")
		lock, err := readCaptureLock(lockPath)
		if err != nil || lock.PID != os.Getpid() || !lock.StartedAt.Equal(now) || lock.BgID != "1a2b3c4d" {
			t.Fatal(lock, err)
		}
		info, err := os.Stat(lockPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(info, err)
		}
		data, _ := os.ReadFile(log + ".args")
		args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
		tmpDirs, err := filepath.Glob(filepath.Join(a.metadataDir(), "capture-tmp", "*"))
		if err != nil || len(tmpDirs) != 1 {
			t.Fatal(tmpDirs, err)
		}
		tmp, err := filepath.EvalSymlinks(tmpDirs[0])
		if err != nil {
			t.Fatal(err)
		}
		if args[0] != "--bg" || !slices.Contains(args, "--agent") || !slices.Contains(args, "kioku:kioku-capture") {
			t.Fatal(args)
		}
		command := testKiokuCommand(a.Config.Server)
		project := command + " -C " + shellQuote(a.Dir) + " -n org/a"
		for _, rule := range []string{"Bash(" + command + " fetch:*)", "Bash(" + project + " put:*)", "Bash(" + project + " queue remove:*)", "Bash(" + project + " capture unlock:*)", "Bash(gh pr view:*)", "Bash(gh issue view:*)", "Bash(gh pr diff:*)", "Read(" + tmp + "/**)", "Write(" + tmp + "/**)"} {
			if !slices.Contains(args, rule) {
				t.Fatal("missing permission", rule, args)
			}
		}
		for _, rule := range args[6 : len(args)-3] {
			if strings.Contains(rule, "curl") || rule == "Bash(gh:*)" || strings.Contains(rule, "gh api") || rule == "WebFetch" {
				t.Fatal("broad permission", rule)
			}
		}

		prompt := args[len(args)-1]
		command = project + " "
		for _, part := range []string{"一時ディレクトリ: " + tmp + "（資料はここに書く）", "server: https://other.example", "P: " + a.Dir, command + "queue remove <URL>", command + "capture unlock", string(before)} {
			if !strings.Contains(prompt, part) {
				t.Fatal("missing", part, prompt)
			}
		}
		data, _ = os.ReadFile(log + ".cwd")
		resolved, _ := filepath.EvalSymlinks(cwd)
		actualCwd, cwdErr := filepath.EvalSymlinks(strings.TrimSpace(string(data)))
		if cwdErr != nil || actualCwd != resolved {
			t.Fatal(string(data), resolved)
		}
		data, _ = os.ReadFile(log + ".sid")
		if string(data) != "unset" {
			t.Fatal(string(data))
		}
		after, _ := os.ReadFile(queue)
		if !bytes.Equal(before, after) {
			t.Fatal("launch removed queue")
		}
	})
	for _, age := range []time.Duration{10 * time.Minute, 30 * time.Minute, 30*time.Minute + time.Second, 40 * time.Minute} {
		t.Run("E36_I7/"+age.String(), func(t *testing.T) {
			a := setup(t, 5)
			log := fakeCaptureClaude(t, "backgrounded · 1a2b3c4d\n", 0)
			now := time.Now().UTC()
			path := filepath.Join(a.metadataDir(), "capture.lock")
			old := captureLock{PID: 123, StartedAt: now.Add(-age), BgID: "aaaaaaaa"}
			writeHookFixture(t, path, old)
			if err := a.captureRun(5, t.TempDir(), now); err != nil {
				t.Fatal(err)
			}
			busy := age <= 30*time.Minute
			data, err := os.ReadFile(log + ".args")
			if busy != os.IsNotExist(err) {
				t.Fatal(string(data), err)
			}
			lock, err := readCaptureLock(path)
			if err != nil {
				t.Fatal(err)
			}
			if busy && !reflect.DeepEqual(lock, &old) || !busy && lock.BgID != "1a2b3c4d" {
				t.Fatal(lock)
			}
			if busy && !strings.Contains(a.Out.(*bytes.Buffer).String(), "already running (id aaaaaaaa") {
				t.Fatal(a.Out)
			}
		})
	}
	t.Run("I14_missing_id", func(t *testing.T) {
		a := setup(t, 5)
		log := fakeCaptureClaude(t, "started without id\n", 0)
		last := filepath.Join(a.metadataDir(), "capture.last")
		if err := os.WriteFile(last, []byte("aaaaaaaa\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.captureRun(5, t.TempDir(), time.Now()); err != nil {
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
			err := a.captureRun(5, t.TempDir(), time.Now())
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
	t.Run("E14_E36_cleanup_and_unlock", func(t *testing.T) {
		a := setup(t, 5)
		log := fakeCaptureClaude(t, "backgrounded · 1a2b3c4d\n", 0)
		now := time.Now().UTC()
		path := filepath.Join(a.metadataDir(), "capture.lock")
		last := filepath.Join(a.metadataDir(), "capture.last")
		writeHookFixture(t, path, captureLock{PID: 123, StartedAt: now.Add(-40 * time.Minute), BgID: "aaaaaaaa"})
		if err := os.WriteFile(last, []byte("bbbbbbbb\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.captureRun(5, t.TempDir(), now); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(log + ".rm")
		if string(data) != "bbbbbbbb\n" {
			t.Fatal(string(data))
		}
		if _, err := os.Stat(last); !os.IsNotExist(err) {
			t.Fatal(err)
		}
		lock, _ := readCaptureLock(path)
		a.Out.(*bytes.Buffer).Reset()
		for range 2 {
			if err := a.capture([]string{"unlock", "--token", lock.Token}, now); err != nil {
				t.Fatal(err)
			}
		}
		if a.Out.(*bytes.Buffer).Len() != 0 || a.Err.(*bytes.Buffer).Len() != 0 {
			t.Fatal(a.Out, a.Err)
		}
		data, _ = os.ReadFile(last)
		if string(data) != "1a2b3c4d\n" {
			t.Fatal(string(data))
		}
		info, err := os.Stat(last)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(info, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal(err)
		}
	})
	t.Run("E14_unlock_before_id", func(t *testing.T) {
		a := setup(t, 5)
		now := time.Now().UTC()
		path := filepath.Join(a.metadataDir(), "capture.lock")
		lock, err := acquireCaptureLock(path, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.captureUnlock(lock.Token); err != nil {
			t.Fatal(err)
		}
		if err := a.recordCaptureID(path, lock.Token, "1a2b3c4d"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("recreated unlocked lock", err)
		}
		data, err := os.ReadFile(filepath.Join(a.metadataDir(), "capture.last"))
		if err != nil || string(data) != "1a2b3c4d\n" {
			t.Fatal(string(data), err)
		}
	})
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
		for _, tc := range []struct {
			name, path, out string
			ok              bool
		}{
			{"https", "/ok", "ok", true}, {"https_redirect", "/redirect", "redirect", true}, {"downgrade", "/downgrade", "bad", false}, {"size_header", "/large", "large", false}, {"size_chunked", "/chunked", "chunked", false}, {"escape", "/ok", "../outside", false},
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
		t.Setenv("KIOKU_CAPTURE_DIR", tmp)
		if err := a.put([]string{outside}); err == nil {
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

		if err := fetchCommand(context.Background(), []string{server.URL, "--out", "x", "--dir", filepath.Dir(outside)}); err == nil {
			t.Fatal("accepted foreign --dir")
		}
		if err := fetchURL(context.Background(), nil, "http://example.invalid", tmp, "http"); err == nil {
			t.Fatal("accepted http")
		}
		for _, server := range []string{defaultServer, "https://other.example"} {
			exe := "/tmp/space and 'quote/kioku"
			rules := captureAllowedTools(exe, server, "org/a", a.Dir, tmp)
			wantConnectors := []string{"mcp__claude_ai_Notion__notion-fetch", "mcp__claude_ai_Notion__notion-search", "mcp__claude_ai_Slack__slack_read_thread", "mcp__claude_ai_Slack__slack_read_channel", "mcp__claude_ai_Google_Drive__read_file_content", "mcp__claude_ai_Google_Drive__download_file_content"}
			var connectors []string
			for _, rule := range rules {
				if strings.HasPrefix(rule, "mcp__") {
					connectors = append(connectors, rule)
				}
			}
			if !reflect.DeepEqual(connectors, wantConnectors) {
				t.Fatal(connectors)
			}
			if rules[0] != "Bash("+kiokuCommand(exe, server)+" fetch:*)" {
				t.Fatal(rules)
			}
			for _, rule := range rules {
				if strings.Contains(rule, "gh api") || strings.Contains(rule, "gh auth") || strings.Contains(rule, "curl") || strings.Contains(rule, "send") || strings.Contains(rule, "create") || strings.Contains(rule, "update") {
					t.Fatal(rule)
				}
			}
		}
	})
	t.Run("R3_R8_ownership_and_tmp_cleanup", func(t *testing.T) {
		a := setup(t, 5)
		log := fakeCaptureClaude(t, "backgrounded · 1a2b3c4d\n", 0)
		now := time.Now()
		path := filepath.Join(a.metadataDir(), "capture.lock")
		old, err := acquireCaptureLock(path, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		old.BgID = "aaaaaaaa"
		writeHookFixture(t, path, old)
		oldTmp := a.captureTmp(old.Token)
		if err := os.MkdirAll(oldTmp, 0700); err != nil {
			t.Fatal(err)
		}
		if err := a.captureRun(1, t.TempDir(), now); err != nil {
			t.Fatal(err)
		}
		current, _ := readCaptureLock(path)
		if current.Token == old.Token || !captureTokenPattern.MatchString(current.Token) {
			t.Fatal(current)
		}
		if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
			t.Fatal("stale tmp retained", err)
		}
		if _, err := os.Stat(log + ".rm"); !os.IsNotExist(err) {
			t.Fatal("removed stale owner", err)
		}
		if err := a.captureUnlock(old.Token); err != nil {
			t.Fatal(err)
		}
		if got, _ := readCaptureLock(path); !reflect.DeepEqual(got, current) {
			t.Fatal("old owner removed current lock", got)
		}
		if !strings.Contains(a.Out.(*bytes.Buffer).String(), "token does not match") {
			t.Fatal(a.Out)
		}
		if err := a.captureUnlock(current.Token); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(a.captureTmp(current.Token)); !os.IsNotExist(err) {
			t.Fatal("owned tmp retained", err)
		}
	})
	for _, age := range []time.Duration{time.Minute, 30 * time.Minute, 31 * time.Minute} {
		t.Run("R4_corrupt/"+age.String(), func(t *testing.T) {
			a := setup(t, 1)
			path := filepath.Join(a.metadataDir(), "capture.lock")
			now := time.Now()
			if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
				t.Fatal(err)
			}
			_, err := acquireCaptureLock(path, now)
			if age <= 30*time.Minute && !errors.Is(err, errCaptureBusy) || age > 30*time.Minute && err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.captureUnlock("wrong"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal(err)
			}
		})
	}
	t.Run("R8_idle_run_prunes_only_nonlive_tmp", func(t *testing.T) {
		a := setup(t, 0)
		now := time.Now()
		lock, err := acquireCaptureLock(filepath.Join(a.metadataDir(), "capture.lock"), now)
		if err != nil {
			t.Fatal(err)
		}
		tmp, err := a.prepareCaptureTmp(lock.Token)
		if err != nil {
			t.Fatal(err)
		}
		stale := a.captureTmp(strings.Repeat("a", 32))
		if err := os.Mkdir(stale, 0700); err != nil {
			t.Fatal(err)
		}
		if err := a.captureRun(1, t.TempDir(), now); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatal("stale directory retained", err)
		}
		if _, err := os.Stat(tmp); err != nil {
			t.Fatal("live directory removed", err)
		}
	})

	t.Run("R5_R9_missing_id_appends_warning", func(t *testing.T) {
		a := setup(t, 1)
		fakeCaptureClaude(t, "no id", 0)
		if err := a.captureNote("earlier log"); err != nil {
			t.Fatal(err)
		}
		if err := a.captureRun(1, t.TempDir(), time.Now()); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(a.metadataDir(), "capture.log"))
		if err != nil || !strings.HasPrefix(string(data), "earlier log\n") || !strings.Contains(string(data), "id could not be parsed") {
			t.Fatal(string(data), err)
		}
		if !strings.Contains(a.Err.(*bytes.Buffer).String(), "warning:") {
			t.Fatal(a.Err)
		}
		lock, _ := readCaptureLock(filepath.Join(a.metadataDir(), "capture.lock"))
		if lock == nil || lock.Token == "" {
			t.Fatal(lock)
		}
		if err := a.captureUnlock(lock.Token); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("R7_lingering_supervisor", func(t *testing.T) {
		a := setup(t, 1)
		log := fakeCaptureClaude(t, "", 0)
		script := "#!/bin/sh\n/bin/sleep 7 &\nprintf 'backgrounded · 1a2b3c4d\\n'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(filepath.Dir(log), "claude"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := a.captureRun(1, t.TempDir(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 6500*time.Millisecond {
			t.Fatal("blocked on supervisor", elapsed)
		}
		lock, _ := readCaptureLock(filepath.Join(a.metadataDir(), "capture.lock"))
		if lock == nil || lock.BgID != "1a2b3c4d" {
			t.Fatal(lock)
		}
	})
	t.Run("R2_queue_failure_reason", func(t *testing.T) {
		a := setup(t, 1)
		if err := a.queue([]string{"remove", "https://x.example/0", "--reason", "未保存: permission denied"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		rows, err := readQueue(filepath.Join(a.metadataDir(), "queue.tsv"))
		if err != nil || len(rows) != 0 {
			t.Fatal(rows, err)
		}
		data, err := os.ReadFile(filepath.Join(a.metadataDir(), "capture.log"))
		if err != nil || !strings.Contains(string(data), "permission denied") {
			t.Fatal(string(data), err)
		}
	})

}
