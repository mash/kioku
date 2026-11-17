package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestQueueCommands(t *testing.T) {
	t.Run("E12_E20_queue_lifecycle", func(t *testing.T) {
		home := projectRunHome(t)
		a := &App{Dir: t.TempDir(), AuthDir: filepath.Join(home, ".config/kioku/auth"), Config: Config{Server: defaultServer, Namespace: "org/a"}, Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
		if err := ensureDirectory(a.metadataDir()); err != nil {
			t.Fatal(err)
		}
		if err := a.saveProjectConfig(); err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.Local)
		path := filepath.Join(a.metadataDir(), "queue.tsv")
		run := func(args ...string) string {
			t.Helper()
			out, stderr, err := runProjectCLI(append([]string{"-n", "org/a", "queue"}, args...)...)
			if err != nil || stderr != "" {
				t.Fatal(out, stderr, err)
			}
			return out
		}
		if got := run("list"); got != "" {
			t.Fatal(got)
		}
		if got := run("remove", "https://x.example/missing"); got != "" {
			t.Fatal(got)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("remove created missing queue", err)
		}
		if err := a.queue([]string{"add", "https://x.example/1", "読んだ"}, now); err != nil {
			t.Fatal(err)
		}
		if got := run("list"); got != "2026-11-17\thttps://x.example/1\t読んだ\n" {
			t.Fatal(got)
		}
		for _, args := range [][]string{{"add", "http://x.example/2", "読んだ"}, {"add", "https://x.example/2", ""}, {"add", "https://x.example/a\tb", "note"}, {"remove"}, {"list", "extra"}} {
			before, _ := os.ReadFile(path)
			if _, _, err := runProjectCLI(append([]string{"-n", "org/a", "queue"}, args...)...); err == nil {
				t.Fatal(args)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("invalid args changed queue")
			}
		}
		if err := a.queue([]string{"add", "https://x.example/2", "other"}, now); err != nil {
			t.Fatal(err)
		}
		if err := a.queue([]string{"add", "https://x.example/1", "結論を変えた"}, now.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got := run("list"); got != "2026-11-17\thttps://x.example/2\tother\n2026-11-18\thttps://x.example/1\t結論を変えた\n" {
			t.Fatal(got)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(info, err)
		}
		if got := run("remove", "https://x.example/1", "https://x.example/missing"); got != "" {
			t.Fatal(got)
		}
		if got := run("list"); got != "2026-11-17\thttps://x.example/2\tother\n" {
			t.Fatal(got)
		}
		run("remove", "https://x.example/2")
		if got := run("list"); got != "" {
			t.Fatal(got)
		}
	})
	t.Run("I9_note_sanitization_and_pure_rows", func(t *testing.T) {
		original := []queueRow{{"2026-11-17", "https://x.example/1", "before"}, {"2026-11-17", "https://x.example/2", "keep"}}
		got := addRow(original, "2026-11-18", "https://x.example/1", "a\tb\nc\rd")
		want := []queueRow{original[1], {"2026-11-18", "https://x.example/1", "a b c d"}}
		if !reflect.DeepEqual(got, want) || original[0].Note != "before" {
			t.Fatal(got, original)
		}
		parsed, err := parseQueue(formatQueue(got))
		if err != nil || !reflect.DeepEqual(parsed, got) {
			t.Fatal(parsed, err)
		}
	})
	t.Run("E12_unbound", func(t *testing.T) {
		projectRunHome(t)
		_, _, err := runProjectCLI("-n", "org/missing", "queue", "list")
		if err == nil || !strings.Contains(err.Error(), "no project configured") {
			t.Fatal(err)
		}
	})
}
