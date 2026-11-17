package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const listHeaderText = "Kioku: このセッションは project が未選択。このメッセージの内容と各 description から最も合う project を、ユーザーに聞かずに 1 つ選び、%[1]s -n <namespace> session select を 1 回実行する。同じくらい合う project が複数あっても 1 つに決める。［server: …］の付いた行は %[1]s --server <server> -n <namespace> session select を使う。どれも合わなければ %[1]s session select --none を実行し、Kioku なしで進める。select が失敗したら kioku skill の失敗時の手順に従う。［ヒント: trigger 語 …］は、このメッセージかブランチ名が、その project の trigger 語に当たったことを示す。"
const turnRuleText = "Kioku: %[1]s（wiki: %[2]s/wiki/、入口は index.md）。調べる時はここを grep する。参照・作成した重要な文書の URL は %[3]s -C %[4]s -n %[1]s queue add <URL> \"<何をしたか>\" で 1 行足す。共有範囲・機密・保存禁止を確かめてから足す。"
const failedPullText = "最後の pull は失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。"
const startHookText = "Kioku: この会話で選んだ project があれば保持する。未選択なら、hook が最初のメッセージを trigger 語に照合する。1 project だけに当たれば hook が選び、wiki の入口を渡す。ほかの時は hook が project の一覧を出す。一覧に示された CLI の絶対パスと手順に従い、ユーザーに聞かずに project を選ぶか Kioku なしを選ぶ。\n"

func candidateTestDir(t *testing.T) string {
	t.Helper()
	boundary := t.TempDir()
	writeHookFixture(t, filepath.Join(boundary, ".kioku/config"), Config{Server: defaultServer, Namespace: "boundary/unused"})
	if err := os.Mkdir(filepath.Join(boundary, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(boundary, ".git/HEAD"), []byte(strings.Repeat("a", 40)), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(boundary, "workspace")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testKiokuCommand(server string) string {
	exe, _ := os.Executable()
	return kiokuCommand(exe, server)
}
func testTurnRule(ns, p, server string) string {
	return fmt.Sprintf(turnRuleText, ns, p, testKiokuCommand(server), shellQuote(p))
}
func testHintList(ns string) string {
	return fmt.Sprintf(listHeaderText, testKiokuCommand(defaultServer)) + "\n- " + ns + ": （説明なし）［ヒント: cwd の .kioku/config］"
}

func writeHookFixture(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func runPromptHook(t *testing.T, input any) string {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return runRawPromptHook(t, string(b), "../../prompts")
}

func runRawPromptHook(t *testing.T, input, prompts string) string {
	t.Helper()
	var out, stderr bytes.Buffer
	a := &App{Out: &out, Err: &stderr}
	if err := a.sessionHook([]string{"--event", "UserPromptSubmit", "--prompts", prompts}, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatal(stderr.String())
	}
	if out.Len() == 0 {
		return ""
	}
	var result struct {
		Output struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Output.Event != "UserPromptSubmit" {
		t.Fatal(result.Output.Event)
	}
	return result.Output.Context
}

func TestPromptHookAcceptanceExamples(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, config, branch, selected, auto  string
		declined, child, noWiki, blocked, other, many bool
	}{
		{name: "E1_list", prompt: "テスト通して"},
		{name: "E2_namespace_hint", prompt: "org/b を続けて"},
		{name: "E3_cwd_hint", prompt: "続けて", config: "org/a"},
		{name: "E4_child", prompt: "Tesla", child: true},
		{name: "E10_declined_namespace_hint", prompt: "次は org/b の件", declined: true},
		{name: "E11_selected", prompt: "続けて", selected: "org/a"},
		{name: "E21_list_limit", prompt: "続けて", many: true},
		{name: "E23_two_triggers", prompt: "Tesla の充電が止まる件"},
		{name: "E24_auto_select", prompt: "Tesla のログを見て", auto: "org/a"},
		{name: "E26_without_wiki", prompt: "Tesla", auto: "org/a", noWiki: true},
		{name: "E27_word_boundary", prompt: "teslax のログ"},
		{name: "E28_substring", prompt: "急速充電器の件", auto: "org/b"},
		{name: "R4_prompt_and_branch_same_hit", prompt: "Tesla", branch: "feat/tesla-x", auto: "org/a"},
		{name: "R4_prompt_and_branch_conflicting_hits", prompt: "充電", branch: "feat/tesla-x"},
		{name: "E29_branch", prompt: "続けて", branch: "feat/tesla-x", auto: "org/a"},
		{name: "E30_selected_ignores_triggers", prompt: "充電の件も", selected: "org/a"},
		{name: "E31_declined_auto_select", prompt: "Tesla", declined: true, auto: "org/a"},
		{name: "E32_declined_multiple", prompt: "Tesla の充電", declined: true},
		{name: "I11_unreadable_state", prompt: "Tesla", blocked: true},
		{name: "I13_other_server", prompt: "Tesla", other: true, auto: "org/a"},
		{name: "R14_declined_namespace_ignores_branch", prompt: "org/b を続けて", declined: true, branch: "feat/tesla-x"},
		{name: "R14_declined_prompt_ignores_conflicting_branch", prompt: "Tesla", declined: true, branch: "feat/充電", auto: "org/a"},
		{name: "I15_declined_without_prompt_hits", prompt: "続けて", declined: true, branch: "feat/tesla-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.blocked && os.Geteuid() == 0 {
				t.Skip("root bypasses permissions")
			}
			home := sessionTestEnv(t)
			cwd := candidateTestDir(t)
			ps := []projectEntry{{Server: defaultServer, Namespace: "org/a", Path: t.TempDir(), Description: "A 案件の説明", Triggers: []string{"tesla"}}, {Server: defaultServer, Namespace: "org/b", Path: t.TempDir(), Triggers: []string{"充電"}}}
			if tc.other {
				ps[0].Server = "https://other.example"
			}
			if tc.many {
				ps = nil
				for i := range 200 {
					ps = append(ps, projectEntry{Server: defaultServer, Namespace: fmt.Sprintf("org/p%d", i), Path: cwd, Description: strings.Repeat("説", 100)})
				}
			}
			for _, p := range ps {
				if !tc.noWiki {
					if err := os.MkdirAll(filepath.Join(p.Path, "wiki"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(p.Path, "wiki/index.md"), []byte("# Index"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.config != "" {
				writeHookFixture(t, filepath.Join(cwd, ".kioku/config"), Config{Server: defaultServer, Namespace: tc.config})
			}
			if tc.branch != "" {
				if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cwd, ".git/HEAD"), []byte("ref: refs/heads/"+tc.branch+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			catalog := filepath.Join(home, ".config/kioku/projects.json")
			writeHookFixture(t, catalog, projectsFile{Projects: ps})
			catalogBefore, _ := os.ReadFile(catalog)
			catalogInfo, _ := os.Stat(catalog)
			statePath, _ := sessionPath("session-1")
			if tc.declined {
				writeHookFixture(t, statePath, &sessionState{DeclinedAt: time.Now().UTC()})
			}
			if tc.selected != "" || tc.blocked {
				s := sessionTestState(time.Now().UTC())
				s.Namespace = "org/a"
				s.Path = ps[0].Path
				writeHookFixture(t, statePath, s)
			}
			before, _ := os.ReadFile(statePath)
			info, _ := os.Stat(statePath)
			if tc.blocked {
				if err := os.Chmod(statePath, 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(statePath, 0600) })
			}
			oldStart := startProcess
			t.Cleanup(func() { startProcess = oldStart })
			var started []*exec.Cmd
			startProcess = func(c *exec.Cmd) error { started = append(started, c); return nil }
			t.Setenv("CLAUDE_CODE_SESSION_ID", "wrong-environment-id")
			t.Setenv("KIOKU_SESSION_PULL", "old")
			input := map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "cwd": cwd, "prompt": tc.prompt}
			if tc.child {
				input["agent_id"] = "child-1"
			}
			got := runPromptHook(t, input)
			if tc.name == "I13_other_server" {
				if !strings.Contains(got, testKiokuCommand(defaultServer)+" session select --none") || strings.Contains(got, testKiokuCommand("https://other.example")+" session select --none") {
					t.Fatal("decline carries global flags", got)
				}
			}
			if tc.blocked {
				_ = os.Chmod(statePath, 0600)
			}
			after, _ := os.ReadFile(statePath)
			if tc.auto != "" {
				s := mustSession(t)
				p := ps[0]
				if tc.auto == "org/b" {
					p = ps[1]
				}
				if s.Namespace != tc.auto || s.Path != p.Path || s.Server != p.Server || s.LastPullResult != sessionPullNone || s.declined() {
					t.Fatal(s)
				}
				if len(started) != 1 {
					t.Fatal(started)
				}
				exe, _ := os.Executable()
				c := started[0]
				if !reflect.DeepEqual(c.Args, []string{exe, "--server", p.Server, "-n", p.Namespace, "pull"}) || c.Dir != p.Path || c.Stdin != nil || c.Stdout != c.Stderr {
					t.Fatal(c)
				}
				if !slices.Contains(c.Env, "CLAUDE_CODE_SESSION_ID=session-1") || !slices.Contains(c.Env, "KIOKU_SESSION_PULL=background") || slices.Contains(c.Env, "CLAUDE_CODE_SESSION_ID=wrong-environment-id") || slices.Contains(c.Env, "KIOKU_SESSION_PULL=old") {
					t.Fatal(c.Env)
				}
				log := c.Stdout.(*os.File).Name()
				logInfo, err := os.Stat(log)
				if err != nil || log != filepath.Join(p.Path, ".kioku/pull.log") || logInfo.Mode().Perm() != 0600 {
					t.Fatal(log, err)
				}
				if !strings.Contains(got, "理由: trigger 語 "+p.Triggers[0]) || !strings.Contains(got, testTurnRule(p.Namespace, p.Path, p.Server)) {
					t.Fatal(got)
				}
				if tc.noWiki {
					if !strings.HasPrefix(got, "wiki は背景で取得中。") {
						t.Fatal(got)
					}
				} else if !strings.HasPrefix(got, "# "+p.Path+"/wiki/index.md\n# Index\n") {
					t.Fatal(got)
				}
			} else {
				if len(started) != 0 || !bytes.Equal(before, after) {
					t.Fatal("unexpected selection", got)
				}
				if info != nil {
					afterInfo, _ := os.Stat(statePath)
					if !info.ModTime().Equal(afterInfo.ModTime()) {
						t.Fatal("refreshed state")
					}
				}
				switch {
				case tc.child:
					if got != "" {
						t.Fatal(got)
					}
				case tc.selected != "":
					if got != testTurnRule("org/a", ps[0].Path, defaultServer) {
						t.Fatal(got)
					}
				case tc.declined:
					expected := "Kioku: このセッションは Kioku なしで進めている。話題が project に合ったら " + testKiokuCommand(defaultServer) + " -n <namespace> session select で選ぶ。一覧は " + testKiokuCommand(defaultServer) + " session list が出す。"
					if tc.name == "E10_declined_namespace_hint" || tc.name == "R14_declined_namespace_ignores_branch" {
						expected += "［ヒント: プロンプトに namespace］org/b"
					}
					if tc.name == "E32_declined_multiple" {
						expected += "［ヒント: trigger 語 tesla］org/a、［ヒント: trigger 語 充電］org/b"
					}
					if got != expected {
						t.Fatal(got)
					}
				case tc.many:
					if !strings.Contains(got, "…ほか ") {
						t.Fatal(got)
					}
				default:
					aLine := "- org/a: A 案件の説明"
					bLine := "- org/b: （説明なし）"
					if tc.name == "E2_namespace_hint" {
						bLine += "［ヒント: プロンプトに namespace］"
					}
					if tc.config != "" {
						aLine += "［ヒント: cwd の .kioku/config］"
					}
					if tc.name == "E23_two_triggers" || tc.name == "R4_prompt_and_branch_conflicting_hits" {
						aLine += "［ヒント: trigger 語 tesla］"
						bLine += "［ヒント: trigger 語 充電］"
					}
					if tc.blocked {
						aLine += "［ヒント: trigger 語 tesla］"
					}
					want := fmt.Sprintf(listHeaderText, testKiokuCommand(defaultServer)) + "\n" + aLine + "\n" + bLine
					if got != want {
						t.Fatalf("got %q\nwant %q", got, want)
					}
				}
			}
			if utf8.RuneCountInString(got) > contextLimit {
				t.Fatal("context exceeds limit")
			}
			catalogAfter, _ := os.ReadFile(catalog)
			updated, _ := os.Stat(catalog)
			if !bytes.Equal(catalogBefore, catalogAfter) || !catalogInfo.ModTime().Equal(updated.ModTime()) {
				t.Fatal("hook changed catalog")
			}
		})
	}
}

func TestPromptHookSelectionTransitions(t *testing.T) {
	for _, scenario := range []string{"E25_pending", "E25_success", "E25_failure", "I12_start_failure"} {
		t.Run(scenario, func(t *testing.T) {
			home := sessionTestEnv(t)
			root := candidateTestDir(t)
			writeHookFixture(t, filepath.Join(home, ".config/kioku/projects.json"), projectsFile{Projects: []projectEntry{{Server: defaultServer, Namespace: "org/a", Path: root, Triggers: []string{"tesla"}}}})
			oldStart := startProcess
			t.Cleanup(func() { startProcess = oldStart })
			calls := 0
			startProcess = func(*exec.Cmd) error {
				calls++
				if scenario == "I12_start_failure" {
					return errors.New("launch failed")
				}
				return nil
			}
			input := map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "cwd": root, "prompt": "Tesla"}
			initial := runPromptHook(t, input)
			if scenario == "I12_start_failure" && !strings.Contains(initial, failedPullText) {
				t.Fatal(initial)
			}
			a := &App{backgroundPull: true, Dir: root, Config: Config{Server: defaultServer, Namespace: "org/a"}}
			switch scenario {
			case "E25_success":
				a.recordSessionPull(nil)
			case "E25_failure":
				a.recordSessionPull(errors.New("network"))
			}
			input["prompt"] = "続けて"
			got := runPromptHook(t, input)
			want := testTurnRule("org/a", root, defaultServer)
			switch scenario {
			case "E25_pending":
				want += "\n" + sessionPullPendingContext
			case "E25_failure", "I12_start_failure":
				want += "\n" + failedPullText
			}
			if got != want || calls != 1 {
				t.Fatal(got, calls)
			}
		})
	}
}

func TestPromptHookE19MalformedPreservesState(t *testing.T) {
	sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/work/p"}
	a.recordSessionPull(nil)
	path, _ := sessionPath("session-1")
	before, _ := os.ReadFile(path)
	for _, input := range []string{"{", `{"session_id":"../escape","hook_event_name":"UserPromptSubmit"}`, `{"session_id":"session-1","hook_event_name":"UserPromptSubmit","agent_id":null}`} {
		if got := runRawPromptHook(t, input, "../../prompts"); got != "" {
			t.Fatal(got)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid input changed state")
		}
	}
}

func TestSessionStartE21ExactText(t *testing.T) {
	t.Run("E15_startup", func(t *testing.T) {
		b, err := os.ReadFile("../../prompts/hook-start.txt")
		if err != nil || string(b) != startHookText {
			t.Fatal(string(b), err)
		}
	})
	t.Run("E22_declined_compact_resume", func(t *testing.T) {
		sessionTestEnv(t)
		now := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
		if err := saveSession("session-1", time.Now(), &sessionState{DeclinedAt: now}); err != nil {
			t.Fatal(err)
		}
		for _, source := range []string{"compact", "resume"} {
			var out bytes.Buffer
			a := &App{Out: &out, Err: io.Discard}
			in := fmt.Sprintf(`{"session_id":"session-1","hook_event_name":"SessionStart","source":%q}`, source)
			if err := a.sessionHook([]string{"--prompts", "../../prompts"}, strings.NewReader(in)); err != nil {
				t.Fatal(err)
			}
			var got map[string]map[string]string
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := "Kioku: このセッションは Kioku なし（2026-11-17T01:00:00Z に決めた）。話題が project に合ったら " + testKiokuCommand(defaultServer) + " -n <namespace> session select で選ぶ。"
			if got["hookSpecificOutput"]["additionalContext"] != want {
				t.Fatal(got)
			}
		}
	})
}

func TestCandidateDiscoveryHandlesBrokenFiles(t *testing.T) {
	home := sessionTestEnv(t)
	catalog := filepath.Join(home, ".config/kioku/projects.json")
	root := candidateTestDir(t)
	config := filepath.Join(root, ".kioku/config")
	writeHookFixture(t, config, Config{Server: defaultServer, Namespace: "org/p"})
	input := map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "cwd": root, "prompt": "続けて"}
	for _, contents := range []string{"missing", "{", `{"projects":[]}`} {
		t.Run(contents, func(t *testing.T) {
			if contents != "missing" {
				writeHookFixture(t, catalog, projectsFile{})
				if err := os.WriteFile(catalog, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := runPromptHook(t, input); got != testHintList("org/p") {
				t.Fatal(got)
			}
		})
	}
	if err := os.WriteFile(config, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := runPromptHook(t, input); got != testHintList("boundary/unused") {
		t.Fatal(got)
	}
}

func TestPromptHookE26WalksAboveGitRoot(t *testing.T) {
	sessionTestEnv(t)
	outer := candidateTestDir(t)
	root := filepath.Join(outer, "repo")
	cwd := filepath.Join(root, "src")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	writeHookFixture(t, filepath.Join(outer, ".kioku/config"), Config{Server: defaultServer, Namespace: "org/p"})
	input := map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "prompt": "続けて", "cwd": cwd}
	if got := runPromptHook(t, input); got != testHintList("org/p") {
		t.Fatal(got)
	}
	writeHookFixture(t, filepath.Join(root, ".kioku/config"), Config{Server: defaultServer, Namespace: "org/nearest"})
	if got := runPromptHook(t, input); got != testHintList("org/nearest") {
		t.Fatal(got)
	}
}

func TestCandidateDiscoveryMatchesOnlyFullNamespacesAndConfig(t *testing.T) {
	for _, tc := range []struct {
		word, text string
		want       bool
	}{
		{"SES", "uses と ses の設定", true}, {"SES", "uses", false}, {"tesla", "teslax", false}, {"充電", "急速充電器", true}, {"fleet telemetry", "Fleet Telemetry ログ", true}, {"tesla", "feat/tesla-x", true}, {"SES", "SES123", false}, {"EV充電", "ev充電器", true}, {"tesla", "_tesla_", true},
	} {
		t.Run("I1_E27_E28/"+tc.word+"/"+tc.text, func(t *testing.T) {
			hits, _ := matchTriggers([]projectEntry{{Namespace: "org/a", Triggers: []string{tc.word}}}, tc.text)
			if (len(hits) > 0) != tc.want {
				t.Fatal(hits)
			}
		})
	}
	for _, kind := range []string{"directory", "worktree_absolute", "worktree_relative", "detached", "outside"} {
		t.Run("I2_I3/"+kind, func(t *testing.T) {
			root := candidateTestDir(t)
			git := filepath.Join(root, ".git")
			if kind != "outside" {
				if strings.HasPrefix(kind, "worktree") {
					git = filepath.Join(root, "metadata")
					if err := os.Mkdir(git, 0700); err != nil {
						t.Fatal(err)
					}
					target := git
					if kind == "worktree_relative" {
						target = "metadata"
					}
					if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+target+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(git, 0700); err != nil {
					t.Fatal(err)
				}
				head := "ref: refs/heads/feat/tesla-x\n"
				if kind == "detached" {
					head = strings.Repeat("a", 40) + "\n"
				}
				if err := os.WriteFile(filepath.Join(git, "HEAD"), []byte(head), 0600); err != nil {
					t.Fatal(err)
				}
			}
			branch, _ := walkAncestors(root)
			want := "feat/tesla-x"
			if kind == "detached" || kind == "outside" {
				want = ""
			}
			if branch != want {
				t.Fatal(branch)
			}
		})
	}
	t.Run("I8_large_entry", func(t *testing.T) {
		root := candidateTestDir(t)
		if err := os.Mkdir(filepath.Join(root, "wiki"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "wiki/index.md"), []byte(strings.Repeat("文", 20000)), 0600); err != nil {
			t.Fatal(err)
		}
		files, err := readEntryFiles(root)
		if err != nil {
			t.Fatal(err)
		}
		got := fitEntry(files, "instructions", contextLimit)
		if utf8.RuneCountInString(got) > contextLimit || !strings.Contains(got, "以降を省いた") {
			t.Fatal(got)
		}
	})
}
