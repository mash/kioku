package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
		if a.Out.(*bytes.Buffer).Len() != 0 {
			t.Fatal(a.Out)
		}
		if _, err := os.Stat(log + ".args"); !os.IsNotExist(err) {
			t.Fatal("below threshold launched", err)
		}
		if err := a.capture([]string{"run", "--min", "1"}, now); err != nil {
			t.Fatal(err)
		}
		if got := a.Out.(*bytes.Buffer).String(); got != "kioku-capture を背景で起動した（org/a、3 件、id 1a2b3c4d）。claude agents で見る。\n" {
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
		exe, _ := os.Executable()
		tmpDirs, err := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "kioku-capture-*"))
		if err != nil || len(tmpDirs) != 1 {
			t.Fatal(tmpDirs, err)
		}
		tmp, err := filepath.EvalSymlinks(tmpDirs[0])
		if err != nil {
			t.Fatal(err)
		}
		wantPrefix := []string{"--bg", "--settings", `{"disableAllHooks":true}`, "--permission-mode", "dontAsk", "--allowedTools", "Bash(gh:*)", "Bash(curl:*)", "Bash(" + exe + ":*)", "Read(" + tmp + "/**)", "Write(" + tmp + "/**)", "WebFetch", "--agent", "kioku:kioku-capture"}
		if len(args) != len(wantPrefix)+1 || !reflect.DeepEqual(args[:len(wantPrefix)], wantPrefix) {
			t.Fatal(args)
		}
		prompt := args[len(args)-1]
		command := testKiokuCommand(a.Config.Server) + " -n org/a "
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
			if busy && a.Out.(*bytes.Buffer).Len() != 0 {
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
		if got := a.Out.(*bytes.Buffer).String(); got != "kioku-capture を背景で起動した（org/a、5 件）。claude agents で見る。\n" {
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
		if string(data) != "aaaaaaaa\nbbbbbbbb\n" {
			t.Fatal(string(data))
		}
		if _, err := os.Stat(last); !os.IsNotExist(err) {
			t.Fatal(err)
		}
		a.Out.(*bytes.Buffer).Reset()
		for range 2 {
			if err := a.capture([]string{"unlock"}, now); err != nil {
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
		if _, err := acquireCaptureLock(path, now); err != nil {
			t.Fatal(err)
		}
		if err := a.captureUnlock(); err != nil {
			t.Fatal(err)
		}
		if err := a.recordCaptureID(path, now, "1a2b3c4d"); err != nil {
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
}
