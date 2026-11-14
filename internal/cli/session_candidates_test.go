package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const singleCandidateText = "Kioku: このセッションではプロジェクトを未選択。候補は %s。作業がこのプロジェクトなら、kioku skill で pull して wiki を読む。違えばユーザーに聞く。"
const multipleCandidatesText = "Kioku: このセッションではプロジェクトを未選択。候補は次のとおり。kioku skill で 1 つ選んで pull する。決められなければユーザーに聞く。"
const turnRuleText = "Kioku: %s（wiki: %s/wiki/）。調べる時はここを grep する。参照・作成した HTTPS URL の原本は kioku-capture に渡す。"
const failedPullText = "最後の pull は失敗。既存コピーを最新と呼ばず、kioku skill の失敗時の手順に従う。"
const startHookText = "Kioku: この会話で選んだプロジェクトがあれば保持する。hook が候補を出す。無ければ kioku skill で選ぶ。最初のメッセージの後、対象を pull して wiki/ を grep とファイルツールで読み、作業を始める。対象が曖昧ならユーザーに聞く。フィードバックだけの依頼では、プロジェクトを設定せず、skill のフィードバック手順に従う。\n"

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

func gitHookFixture(t *testing.T, dir, branch string) {
	t.Helper()
	if out, err := exec.Command("git", "init", "-q", "-b", branch, dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
}

func TestPromptHookAcceptanceExamples(t *testing.T) {
	for _, tt := range []struct {
		name, prompt, config, branch, want        string
		projects                                  []string
		subdir, selected, failed, notified, child bool
	}{
		{name: "E1_cwd_config", prompt: "続けて", config: "org/p", want: fmt.Sprintf(singleCandidateText, "org/p")},
		{name: "E2_parent_config", prompt: "続けて", config: "org/p", subdir: true, want: fmt.Sprintf(singleCandidateText, "org/p")},
		{name: "E3_project_word", prompt: "tesla", projects: []string{"natureglobal/tesla"}, want: fmt.Sprintf(singleCandidateText, "natureglobal/tesla")},
		{name: "E4_partial_word", prompt: "uses", projects: []string{"org/ses"}},
		{name: "E5_namespace_priority", prompt: "a/tesla", projects: []string{"a/tesla", "b/tesla"}, want: fmt.Sprintf(singleCandidateText, "a/tesla")},
		{name: "E6_namespace_over_config", prompt: "org/b", config: "org/a", projects: []string{"org/b"}, want: fmt.Sprintf(singleCandidateText, "org/b")},
		{name: "E7_rule", prompt: "続けて", selected: true},
		{name: "E8_failed_rule", prompt: "続けて", selected: true, failed: true},
		{name: "E9_new_url", prompt: "https://example.com/doc", selected: true},
		{name: "E10_child", prompt: "https://example.com/doc", selected: true, child: true},
		{name: "E11_multiple", prompt: "tesla", projects: []string{"a/tesla", "b/tesla"}, want: multipleCandidatesText + "\na/tesla\nb/tesla"},
		{name: "E12_branch", prompt: "続けて", branch: "feat/tesla", projects: []string{"natureglobal/tesla"}, want: fmt.Sprintf(singleCandidateText, "natureglobal/tesla")},
		{name: "E13_servers", prompt: "org/p", projects: []string{"org/p", "org/p"}, want: multipleCandidatesText + "\norg/p（server=https://kioku.magicians.ee）\norg/p（server=https://other.example）"},
		{name: "E20_description_ignored", prompt: "電気自動車", projects: []string{"org/tesla"}},
		{name: "E22_notified_url", prompt: "https://example.com/doc", selected: true, notified: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := sessionTestEnv(t)
			root := t.TempDir()
			branch := tt.branch
			if branch == "" {
				branch = "main"
			}
			gitHookFixture(t, root, branch)
			cwd := root
			if tt.subdir {
				cwd = filepath.Join(root, "src")
				if err := os.Mkdir(cwd, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if tt.config != "" {
				writeHookFixture(t, filepath.Join(root, ".kioku", "config"), Config{Server: defaultServer, Namespace: tt.config})
			}
			projects := projectsFile{}
			for i, ns := range tt.projects {
				server := defaultServer
				if tt.name == "E13_servers" && i == 1 {
					server = "https://other.example"
				}
				projects.Projects = append(projects.Projects, projectEntry{Server: server, Namespace: ns, Path: root, Description: "電気自動車"})
			}
			catalog := filepath.Join(home, ".config", "kioku", "projects.json")
			writeHookFixture(t, catalog, projects)
			catalogBefore, _ := os.ReadFile(catalog)
			statePath, _ := sessionPath("session-1")
			want := tt.want
			if tt.selected {
				s := sessionTestState(time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
				s.Path = root
				if tt.failed {
					s.LastPullResult = "failure"
				}
				if tt.notified {
					s.NotifiedURLs = []string{"https://example.com/doc"}
				}
				writeHookFixture(t, statePath, s)
				want = fmt.Sprintf(turnRuleText, "org/p", root)
				if tt.failed {
					want += "\n" + failedPullText
				}
				if tt.name == "E9_new_url" {
					want += "\nKioku: org/p（local, server=https://kioku.magicians.ee, P=" + root + "）。親は次の URL を kioku-capture に渡す。kioku-capture が原本の全文を put する。namespace・server・mode・P・CLI の絶対パス・共有範囲・除外条件も渡す。\nhttps://example.com/doc"
				}
			}
			input := map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "cwd": cwd, "prompt": tt.prompt}
			if tt.child {
				input["agent_id"] = "child-1"
				want = ""
			}
			before, _ := os.ReadFile(statePath)
			info, _ := os.Stat(statePath)
			got := runPromptHook(t, input)
			if strings.HasPrefix(want, multipleCandidatesText) {
				gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
				if gotLines[0] != wantLines[0] {
					t.Fatalf("got %q want %q", got, want)
				}
				slices.Sort(gotLines[1:])
				slices.Sort(wantLines[1:])
				if !reflect.DeepEqual(gotLines, wantLines) {
					t.Fatalf("got %q want %q", got, want)
				}
			} else if got != want {
				t.Fatalf("got %q\nwant %q", got, want)
			}
			after, _ := os.ReadFile(statePath)
			if tt.name == "E9_new_url" {
				var expected sessionState
				if err := json.Unmarshal(before, &expected); err != nil {
					t.Fatal(err)
				}
				expected.NotifiedURLs = []string{"https://example.com/doc"}
				if !reflect.DeepEqual(mustSession(t), &expected) {
					t.Fatal("hook changed fields other than notified_urls")
				}
			} else if !bytes.Equal(before, after) {
				t.Fatal("hook changed state")
			} else if info != nil {
				afterInfo, _ := os.Stat(statePath)
				if !info.ModTime().Equal(afterInfo.ModTime()) {
					t.Fatal("hook refreshed state")
				}
			}
			catalogAfter, _ := os.ReadFile(catalog)
			if !bytes.Equal(catalogBefore, catalogAfter) {
				t.Fatal("hook changed projects")
			}
		})
	}
}

func TestPromptHookSelectionTransitions(t *testing.T) {
	for _, scenario := range []string{"E14_success", "E15_initial_failure", "E16_same_failure", "E17_selected_last", "E18_selected_last_failure"} {
		t.Run(scenario, func(t *testing.T) {
			sessionTestEnv(t)
			root := t.TempDir()
			writeHookFixture(t, filepath.Join(root, ".kioku", "config"), Config{Server: defaultServer, Namespace: "org/p"})
			input := map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "cwd": root, "prompt": "続けて"}
			candidate := fmt.Sprintf(singleCandidateText, "org/p")
			for i := 0; i < 2; i++ {
				if got := runPromptHook(t, input); got != candidate {
					t.Fatal(got)
				}
			}
			statePath, _ := sessionPath("session-1")
			if _, err := os.Stat(statePath); !os.IsNotExist(err) {
				t.Fatal("candidate created state", err)
			}
			a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: root, Err: &bytes.Buffer{}}
			want := fmt.Sprintf(turnRuleText, "org/p", root)
			switch scenario {
			case "E14_success":
				a.recordSessionPull(nil)
			case "E15_initial_failure":
				a.recordSessionPull(errors.New("network"))
				want = candidate
			case "E16_same_failure":
				a.recordSessionPull(nil)
				before := mustSession(t).LastPullAt
				a.recordSessionPull(errors.New("network"))
				s := mustSession(t)
				if !s.LastPullAt.After(before) || s.LastPullResult != "failure" {
					t.Fatal(s)
				}
				want += "\n" + failedPullText
			case "E17_selected_last", "E18_selected_last_failure":
				a.Config.Namespace = "org/b"
				a.Dir = filepath.Join(root, "b")
				a.recordSessionPull(nil)
				before, _ := os.ReadFile(statePath)
				a.Config.Namespace = "org/a"
				a.Dir = filepath.Join(root, "a")
				if scenario == "E17_selected_last" {
					a.recordSessionPull(nil)
					want = fmt.Sprintf(turnRuleText, "org/a", a.Dir)
				} else {
					a.recordSessionPull(errors.New("network"))
					want = fmt.Sprintf(turnRuleText, "org/b", filepath.Join(root, "b"))
					after, _ := os.ReadFile(statePath)
					if !bytes.Equal(before, after) {
						t.Fatal("failed switch changed state")
					}
				}
			}
			if got := runPromptHook(t, input); got != want {
				t.Fatalf("got %q want %q", got, want)
			}
			if scenario == "E15_initial_failure" {
				if _, err := os.Stat(statePath); !os.IsNotExist(err) {
					t.Fatal("failed initial pull created state")
				}
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
	if got := runRawPromptHook(t, "{", "../../prompts"); got != "" {
		t.Fatal(got)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("malformed input changed state")
	}
}

func TestSessionStartE21ExactText(t *testing.T) {
	b, err := os.ReadFile("../../prompts/hook-start.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != startHookText {
		t.Fatalf("got %q", b)
	}
}

func TestCandidateWordBoundariesAndDeduplication(t *testing.T) {
	for _, tt := range []struct {
		text string
		want bool
	}{
		{"ses", true}, {"uses", false}, {"sesame", false}, {"a_ses", false}, {"ses_1", false}, {"1ses", false}, {"ses2", false},
		{"uses ses", true}, {"feat/ses", true}, {"fix-ses", true}, {"「ses」の変更", true}, {"SES", false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			if got := containsProjectWord(tt.text, "ses"); got != tt.want {
				t.Fatal(got)
			}
		})
	}
	p := projectEntry{Server: defaultServer, Namespace: "org/ses", Path: "/work/p"}
	other := p
	other.Server = "https://other.example"
	got := projectWordCandidates([]projectEntry{p, p, other}, "ses", "feat/ses")
	if !reflect.DeepEqual(got, []projectEntry{p, other}) {
		t.Fatal(got)
	}
}

func TestCandidateConfigStopsAtGitRootAndKeepsSameStageMatches(t *testing.T) {
	sessionTestEnv(t)
	outer := t.TempDir()
	writeHookFixture(t, filepath.Join(outer, ".kioku", "config"), Config{Server: defaultServer, Namespace: "org/outside"})
	root := filepath.Join(outer, "repo")
	gitHookFixture(t, root, "feat/none")
	cwd := filepath.Join(root, "src")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	if got := sessionCandidates("続けて", cwd); len(got) != 0 {
		t.Fatal("escaped Git root", got)
	}
	writeHookFixture(t, filepath.Join(root, ".kioku", "config"), Config{Server: defaultServer, Namespace: "org/root"})
	if got := sessionCandidates("続けて", cwd); len(got) != 1 || got[0].Namespace != "org/root" {
		t.Fatal(got)
	}
	writeHookFixture(t, filepath.Join(cwd, ".kioku", "config"), Config{Server: defaultServer, Namespace: "org/nearest"})
	if got := sessionCandidates("続けて", cwd); len(got) != 2 || got[0].Namespace != "org/nearest" || got[1].Namespace != "org/root" {
		t.Fatal(got)
	}
	nonGit := filepath.Join(outer, "plain")
	if err := os.Mkdir(nonGit, 0700); err != nil {
		t.Fatal(err)
	}
	if got := sessionCandidates("続けて", nonGit); len(got) != 0 {
		t.Fatal("non-Git searched ancestors", got)
	}
	writeHookFixture(t, filepath.Join(nonGit, ".kioku", "config"), Config{Server: defaultServer, Namespace: "org/plain"})
	if got := sessionCandidates("続けて", nonGit); len(got) != 1 || got[0].Namespace != "org/plain" {
		t.Fatal(got)
	}
}

func TestCandidateDiscoveryHandlesBrokenFilesAndGitTimeout(t *testing.T) {
	home := sessionTestEnv(t)
	catalog := filepath.Join(home, ".config", "kioku", "projects.json")
	writeHookFixture(t, catalog, projectsFile{})
	if err := os.WriteFile(catalog, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeHookFixture(t, filepath.Join(root, ".kioku", "config"), Config{Server: defaultServer, Namespace: "org/p"})
	// A broken catalog does not hide a valid cwd hint.
	if got := sessionCandidates("続けて", root); len(got) != 1 || got[0].Namespace != "org/p" {
		t.Fatal(got)
	}
	if err := os.WriteFile(filepath.Join(root, ".kioku", "config"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := sessionCandidates("続けて", root); len(got) != 0 {
		t.Fatal(got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := hookGit(ctx, root, "rev-parse", "--show-toplevel"); got != "" {
		t.Fatal(got)
	}
}

func TestPromptHookMissingCaptureTemplateKeepsRuleWithoutSaving(t *testing.T) {
	sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/work/p"}
	a.recordSessionPull(nil)
	path, _ := sessionPath("session-1")
	before, _ := os.ReadFile(path)
	prompts := t.TempDir()
	b, err := os.ReadFile("../../prompts/hook-turn.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prompts, "hook-turn.txt"), b, 0600); err != nil {
		t.Fatal(err)
	}
	got := runRawPromptHook(t, `{"session_id":"session-1","hook_event_name":"UserPromptSubmit","prompt":"https://example.com/doc"}`, prompts)
	if got != fmt.Sprintf(turnRuleText, "org/p", "/work/p") {
		t.Fatal(got)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("missing template changed state")
	}
}

func TestPromptHookConcurrentURLsEmitEveryRuleButNotifyOnce(t *testing.T) {
	sessionTestEnv(t)
	a := &App{Config: Config{Server: defaultServer, Namespace: "org/p"}, Dir: "/work/p"}
	a.recordSessionPull(nil)
	input := map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "prompt": "https://example.com/doc"}
	var wg sync.WaitGroup
	outputs := make(chan string, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); outputs <- runPromptHook(t, input) }()
	}
	wg.Wait()
	close(outputs)
	notices := 0
	for out := range outputs {
		if !strings.HasPrefix(out, fmt.Sprintf(turnRuleText, "org/p", "/work/p")) {
			t.Fatal(out)
		}
		if strings.Contains(out, "https://example.com/doc") {
			notices++
		}
	}
	if notices != 1 || !reflect.DeepEqual(mustSession(t).NotifiedURLs, []string{"https://example.com/doc"}) {
		t.Fatal(notices)
	}
}
