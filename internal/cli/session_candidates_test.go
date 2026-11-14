package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
		name, prompt, config, branch, want string
		projects                           []string
		subdir, selected, failed, child    bool
	}{
		{name: "E1_cwd_config", prompt: "続けて", config: "org/p", want: fmt.Sprintf(singleCandidateText, "org/p")},
		{name: "E2_parent_config", prompt: "続けて", config: "org/p", subdir: true, want: fmt.Sprintf(singleCandidateText, "org/p")},
		{name: "E5_namespace_priority", prompt: "a/tesla", projects: []string{"a/tesla", "b/tesla"}, want: fmt.Sprintf(singleCandidateText, "a/tesla")},
		{name: "E6_namespace_over_config", prompt: "org/b", config: "org/a", projects: []string{"org/b"}, want: fmt.Sprintf(singleCandidateText, "org/b")},
		{name: "E7_rule", prompt: "続けて", selected: true},
		{name: "E8_failed_rule", prompt: "続けて", selected: true, failed: true},
		{name: "E9_new_url", prompt: "https://example.com/doc", selected: true},
		{name: "E10_child", prompt: "https://example.com/doc", selected: true, child: true},
		{name: "E11_multiple", prompt: "a/tesla と b/tesla", projects: []string{"a/tesla", "b/tesla"}, want: multipleCandidatesText + "\na/tesla\nb/tesla"},
		{name: "E13_servers", prompt: "org/p", projects: []string{"org/p", "org/p"}, want: fmt.Sprintf(singleCandidateText, "org/p")},
		{name: "E27_multiple_namespaces", prompt: "a/tesla と b/tesla を比較して", projects: []string{"a/tesla", "b/tesla"}, want: multipleCandidatesText + "\na/tesla\nb/tesla"},
		{name: "E28_duplicate_namespace", prompt: "org/p", projects: []string{"org/p", "org/p"}, want: fmt.Sprintf(singleCandidateText, "org/p")},
		{name: "E29_url_rule_only", prompt: "https://example.com/doc を調べて", selected: true},
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
				if (tt.name == "E13_servers" || tt.name == "E28_duplicate_namespace") && i == 1 {
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
					s.LastPullResult = sessionPullFailure
				}
				writeHookFixture(t, statePath, s)
				want = fmt.Sprintf(turnRuleText, "org/p", root)
				if tt.failed {
					want += "\n" + failedPullText
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
			if !bytes.Equal(before, after) {
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
				if !s.LastPullAt.After(before) || s.LastPullResult != sessionPullFailure {
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

func TestCandidateDiscoveryHandlesBrokenFiles(t *testing.T) {
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

}

func TestPromptHookE26WalksAboveGitRoot(t *testing.T) {
	sessionTestEnv(t)
	outer := t.TempDir()
	root := filepath.Join(outer, "repo")
	gitHookFixture(t, root, "feat/tesla")
	cwd := filepath.Join(root, "src")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	writeHookFixture(t, filepath.Join(outer, ".kioku/config"), Config{Server: defaultServer, Namespace: "org/p"})
	got := runPromptHook(t, map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "prompt": "続けて", "cwd": cwd})
	if got != fmt.Sprintf(singleCandidateText, "org/p") {
		t.Fatal(got)
	}
	path, _ := sessionPath("session-1")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("hook created state", err)
	}
	// Stop at the nearest config even when ancestors contain other namespaces.
	writeHookFixture(t, filepath.Join(root, ".kioku/config"), Config{Server: defaultServer, Namespace: "org/nearest"})
	got = runPromptHook(t, map[string]any{"session_id": "session-1", "hook_event_name": "UserPromptSubmit", "prompt": "続けて", "cwd": cwd})
	if got != fmt.Sprintf(singleCandidateText, "org/nearest") {
		t.Fatal(got)
	}
}

func TestCandidateDiscoveryWithoutGit(t *testing.T) {
	home := sessionTestEnv(t)
	root := t.TempDir()
	gitHookFixture(t, root, "feat/tesla")
	writeHookFixture(t, filepath.Join(home, ".config/kioku/projects.json"), projectsFile{Projects: []projectEntry{
		{Server: defaultServer, Namespace: "org/tesla", Path: root, Description: "電気自動車"},
	}})
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho called >> \"$KIOKU_TEST_GIT_LOG\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIOKU_TEST_GIT_LOG", log)
	t.Setenv("PATH", bin)
	for _, prompt := range []string{"tesla", "「tesla」の変更", "電気自動車", "続けて"} {
		if got := sessionCandidates(prompt, root); len(got) != 0 {
			t.Fatal(prompt, got)
		}
	}
	if got := sessionCandidates("org/tesla", root); len(got) != 1 || got[0].Namespace != "org/tesla" {
		t.Fatal(got)
	}
	child := filepath.Join(root, "plain", "src")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	writeHookFixture(t, filepath.Join(root, "plain", ".kioku/config"), Config{Server: defaultServer, Namespace: "org/plain"})
	if got := sessionCandidates("続けて", child); len(got) != 1 || got[0].Namespace != "org/plain" {
		t.Fatal(got)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("hook called git", err)
	}
}
