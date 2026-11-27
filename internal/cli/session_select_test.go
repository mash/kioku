package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSessionSelect(t *testing.T) {
	for _, ns := range []string{"", "invalid"} {
		t.Run("R13_invalid_fallback/"+ns, func(t *testing.T) {
			sessionTestEnv(t)
			a, _, _, _ := syncFixture(t)
			a.Config.Namespace = ns
			if err := ensureDirectory(filepath.Join(a.Dir, "wiki")); err != nil {
				t.Fatal(err)
			}
			if err := a.sessionSelect(nil, time.Now()); err == nil {
				t.Fatal("selected invalid namespace")
			}
			if a.Out.(*bytes.Buffer).Len() != 0 {
				t.Fatal(a.Out)
			}
			path, _ := sessionPath("session-1")
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("wrote session", err)
			}
		})
	}

	t.Run("E8_bounded_stdout", func(t *testing.T) {
		sessionTestEnv(t)
		a, files, _, _ := syncFixture(t)
		(*files)["wiki/overview.md"] = strings.Repeat("文", 12000)
		if err := a.sessionSelect(nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		out := a.Out.(*bytes.Buffer).String()
		if !strings.Contains(out, testKiokuCommand(defaultServer)+" -n <namespace> session select") || strings.Contains(out, testKiokuCommand(a.Config.Server)+" -n <namespace> session select") {
			t.Fatal("correction carries selected server", out)
		}
		if utf8.RuneCountInString(out) > 10000 || !strings.Contains(out, "以降を省いた") || !strings.Contains(out, "返答の冒頭に 1 行だけ書く") {
			t.Fatal(out)
		}
	})
	t.Run("E7_preserve_pull_binding_failure_output", func(t *testing.T) {
		sessionTestEnv(t)
		a, _, _, _ := syncFixture(t)
		a.Config.Namespace = ""
		a.projectNamespace = "org/missing"
		if err := a.pull(); err == nil {
			t.Fatal("expected binding failure")
		}
		if got := a.Out.(*bytes.Buffer).String(); got != "" {
			t.Fatal(got)
		}
	})

	for _, tc := range []struct {
		name                         string
		failure, copy, broken, noSID bool
	}{
		{name: "E5_success"},
		{name: "E6_failure_with_copy", failure: true, copy: true},
		{name: "E7_failure_without_copy", failure: true},
		{name: "E6_broken_credentials_with_copy", copy: true, broken: true},
		{name: "E5_without_sid", noSID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := sessionTestEnv(t)
			a, files, _, status := syncFixture(t)
			a.AuthDir = filepath.Join(home, ".config/kioku/auth")
			if err := ensureDirectory(a.metadataDir()); err != nil {
				t.Fatal(err)
			}
			if err := a.saveProjectConfig(); err != nil {
				t.Fatal(err)
			}
			(*files)["wiki/index.md"] = "# Index"
			(*files)["wiki/overview.md"] = "# Overview"
			if tc.copy {
				if err := ensureDirectory(filepath.Join(a.Dir, "wiki")); err != nil {
					t.Fatal(err)
				}
				for name, body := range *files {
					if err := os.WriteFile(filepath.Join(a.Dir, name), []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.failure {
				*status = 403
			}
			if tc.broken {
				if err := ensureDirectory(a.AuthDir); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(a.credentialPath(), []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.noSID {
				t.Setenv("CLAUDE_CODE_SESSION_ID", "")
			}
			out, stderr, err := runProjectCLI("-C", a.Dir, "session", "select")
			statePath, _ := sessionPath("session-1")
			if tc.failure && !tc.copy {
				if err == nil || out != "" || stderr != "" {
					t.Fatal(out, stderr, err)
				}
				if _, err := os.Stat(statePath); !os.IsNotExist(err) {
					t.Fatal("failed first select wrote state", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			exe, _ := os.Executable()
			want := "# " + a.Dir + "/wiki/index.md\n# Index\n# " + a.Dir + "/wiki/overview.md\n# Overview\n" +
				"Kioku: org/project を選んだ（P=" + a.Dir + "）。上が wiki の入口。本文のページは " + a.Dir + "/wiki/ にあり、index.md から必要なものだけ読む。\n" +
				"返答の冒頭に 1 行だけ書く: 「Kioku: org/project の wiki を読み込みました（理由: <選んだ理由>）」。違うと利用者が言ったら、利用者が示した project で " + kiokuCommand(exe, defaultServer) + " -n <namespace> session select を実行する。"
			result := sessionPullSuccess
			if tc.failure || tc.broken {
				want += "\n" + sessionPullFailedContext
				result = sessionPullFailure
				if !strings.HasPrefix(stderr, "warning: pull failed: ") || !strings.HasSuffix(stderr, "; using the local copy\n") {
					t.Fatal(stderr)
				}
			} else if stderr != "" {
				t.Fatal(stderr)
			}
			if out != want+"\n" {
				t.Fatalf("got %q\nwant %q", out, want+"\n")
			}
			if tc.noSID {
				if _, err := os.Stat(statePath); !os.IsNotExist(err) {
					t.Fatal(err)
				}
			} else {
				s := mustSession(t)
				if s.Namespace != "org/project" || s.Path != a.Dir || s.LastPullResult != result || s.LastPullAt.IsZero() {
					t.Fatal(s)
				}
			}
			if result == sessionPullSuccess {
				again, stderr, err := runProjectCLI("-C", a.Dir, "session", "select")
				if err != nil || stderr != "" || again != out || strings.Contains(again, "最新です") {
					t.Fatal(again, stderr, err)
				}
			}
		})
	}
	t.Run("E16_switch", func(t *testing.T) {
		home := sessionTestEnv(t)
		a, _, _, _ := syncFixture(t)
		a.AuthDir = filepath.Join(home, ".config/kioku/auth")
		if err := ensureDirectory(a.metadataDir()); err != nil {
			t.Fatal(err)
		}
		if err := a.saveProjectConfig(); err != nil {
			t.Fatal(err)
		}
		if err := saveSession("session-1", time.Now(), sessionTestState(time.Now())); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runProjectCLI("-C", a.Dir, "session", "select"); err != nil {
			t.Fatal(err)
		}
		if got := mustSession(t); got.Path != a.Dir || got.Namespace != "org/project" {
			t.Fatal(got)
		}
		path, _ := sessionPath("session-1")
		before, _ := os.ReadFile(path)
		other, _, _, otherStatus := syncFixture(t)
		other.AuthDir = a.AuthDir
		*otherStatus = 403
		if err := ensureDirectory(other.metadataDir()); err != nil {
			t.Fatal(err)
		}
		if err := other.saveProjectConfig(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runProjectCLI("-C", other.Dir, "session", "select"); err == nil {
			t.Fatal("expected failure")
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("failed switch changed state")
		}
	})
	t.Run("E8_I8_entry_limits", func(t *testing.T) {
		for _, size := range []int{10000, 10001, 24000} {
			files := []entryFile{{Path: "/work/p/wiki/index.md", Body: "# Index"}, {Path: "/work/p/wiki/overview.md"}}
			tail := "instruction remains whole"
			overhead := utf8.RuneCountInString(fitEntry(files, tail, contextLimit))
			files[1].Body = strings.Repeat("文", size-overhead)
			got := fitEntry(files, tail, contextLimit)
			if utf8.RuneCountInString(got) > contextLimit || !strings.HasSuffix(got, tail) || !strings.Contains(got, "# Index") {
				t.Fatal(size, len(got))
			}
			if strings.Contains(got, "以降を省いた") != (size > contextLimit) {
				t.Fatal(size, got)
			}
		}
		dir := t.TempDir()
		if err := ensureDirectory(filepath.Join(dir, "wiki")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "wiki/index.md"), []byte(strings.Repeat("文", 20000)), 0600); err != nil {
			t.Fatal(err)
		}
		files, err := readEntryFiles(dir)
		if err != nil || len(files) != 1 || !files[0].Truncated || len(files[0].Body) > contextLimit*4 || !utf8.ValidString(files[0].Body) {
			t.Fatal(files, err)
		}
		got := fitEntry(files, "instruction", contextLimit)
		if utf8.RuneCountInString(got) > contextLimit || !strings.Contains(got, "以降を省いた") {
			t.Fatal(len(got))
		}
		files = append(files, entryFile{Path: dir + "/wiki/overview.md", Body: strings.Repeat("overview", 2000)})
		got = fitEntry(files, "instruction", contextLimit)
		if utf8.RuneCountInString(got) > contextLimit || strings.Count(got, "以降を省いた") != 2 {
			t.Fatal(len(got))
		}
	})
	t.Run("E5_missing_entries", func(t *testing.T) {
		sessionTestEnv(t)
		a, files, _, _ := syncFixture(t)
		delete(*files, "wiki/index.md")
		if err := a.sessionSelect(nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(a.Out.(*bytes.Buffer).String(), "wiki/index.md と overview.md が無い。") {
			t.Fatal(a.Out)
		}
	})
}
