package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProjectTriggersAndSessionList(t *testing.T) {
	setup := func(t *testing.T) *App {
		t.Helper()
		home := projectRunHome(t)
		a := &App{Dir: t.TempDir(), AuthDir: filepath.Join(home, ".config/kioku/auth"), Config: Config{Server: defaultServer, Namespace: "org/a"}}
		if err := ensureDirectory(a.metadataDir()); err != nil {
			t.Fatal(err)
		}
		if err := a.saveProjectConfig(); err != nil {
			t.Fatal(err)
		}
		return a
	}
	t.Run("E33_E34_replace_clear_and_list", func(t *testing.T) {
		a := setup(t)
		desc := "A 案件の説明"
		if err := a.addProjectEntry(defaultServer, "org/a", a.Dir, &desc); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			words []string
			want  string
		}{
			{[]string{" tesla ", "EV充電", "tesla"}, "tesla, EV充電"},
			{nil, "（なし）"},
		} {
			out, stderr, err := runProjectCLI(append([]string{"-n", "org/a", "project", "triggers"}, tc.words...)...)
			if err != nil || stderr != "" || out != "org/a triggers: "+tc.want+"\n" {
				t.Fatal(out, stderr, err)
			}
			list, stderr, err := runProjectCLI("session", "list")
			want := "- org/a: A 案件の説明"
			if len(tc.words) > 0 {
				want += "［trigger 語: tesla、EV充電］"
			}
			if err != nil || stderr != "" || list != want+"\n" {
				t.Fatal(list, stderr, err)
			}
		}
	})
	t.Run("I10_normalization_and_invalid_words", func(t *testing.T) {
		a := setup(t)
		words, err := normalizeTriggers([]string{"Tesla", "tesla", "充電", "充電", "EV充電", "ev充電"})
		if err != nil || !reflect.DeepEqual(words, []string{"Tesla", "充電", "EV充電", "ev充電"}) {
			t.Fatal(words, err)
		}
		for _, word := range []string{"", "  ", "a\tb", "a\nb", "\tword", "word\n", "a\rb"} {
			before, _ := os.ReadFile(a.projectsPath())
			_, _, err := runProjectCLI("-n", "org/a", "project", "triggers", word)
			if err == nil || !strings.Contains(err.Error(), "project triggers: invalid word") {
				t.Fatal(word, err)
			}
			after, _ := os.ReadFile(a.projectsPath())
			if !bytes.Equal(before, after) {
				t.Fatal("invalid word changed catalog")
			}
		}
	})
	t.Run("I4_update_all_paths_and_fold", func(t *testing.T) {
		a := setup(t)
		ps := []projectEntry{
			{Server: defaultServer, Namespace: "org/a", Path: a.Dir, Triggers: []string{"Tesla"}},
			{Server: defaultServer, Namespace: "org/a", Path: t.TempDir(), Description: "A description", Triggers: []string{"tesla", "充電"}},
			{Server: "https://other.example", Namespace: "org/a", Path: t.TempDir(), Triggers: []string{"other"}},
		}
		data, _ := json.Marshal(projectsFile{Projects: ps})
		writeProjectsFixture(t, a, string(data))
		folded := foldProjects(ps)
		if len(folded) != 2 || folded[0].Path != a.Dir || folded[0].Description != "A description" || !reflect.DeepEqual(folded[0].Triggers, []string{"Tesla", "充電"}) {
			t.Fatal(folded)
		}
		out, stderr, err := runProjectCLI("-n", "org/a", "project", "triggers", "new")
		if err != nil || stderr != "" || out != "org/a triggers: new\n" {
			t.Fatal(out, stderr, err)
		}
		saved, err := readProjects(a.projectsPath())
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range saved.Projects {
			want := []string{"new"}
			if i == 2 {
				want = []string{"other"}
			}
			if !reflect.DeepEqual(p.Triggers, want) || p.Path != ps[i].Path {
				t.Fatal(p)
			}
		}
		info, err := os.Stat(a.projectsPath())
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(info, err)
		}
	})
	t.Run("E17_list_and_errors", func(t *testing.T) {
		a := setup(t)
		writeProjectsFixture(t, a, `{"projects":[{"server":"https://kioku.magicians.ee","namespace":"org/a","path":"/work/a","description":"A\n案件"},{"server":"https://other.example","namespace":"org/b","path":"/work/b"}]}`)
		out, stderr, err := runProjectCLI("session", "list")
		if err != nil || stderr != "" || out != "- org/a: A 案件\n- org/b［server: https://other.example］: （説明なし）\n" {
			t.Fatal(out, stderr, err)
		}
		writeProjectsFixture(t, a, "broken")
		for _, args := range [][]string{{"session", "list"}, {"-C", a.Dir, "project", "triggers", "tesla"}} {
			out, _, err := runProjectCLI(args...)
			if err == nil || out != "" {
				t.Fatal(out, err)
			}
			data, _ := os.ReadFile(a.projectsPath())
			if string(data) != "broken" {
				t.Fatal("rewrote invalid catalog")
			}
		}
	})
	t.Run("E33_unbound", func(t *testing.T) {
		projectRunHome(t)
		_, _, err := runProjectCLI("-n", "org/missing", "project", "triggers", "tesla")
		if err == nil || !strings.Contains(err.Error(), "no project configured") {
			t.Fatal(err)
		}
	})
	t.Run("E21_list_limit_and_full_cli", func(t *testing.T) {
		a := setup(t)
		var ps []projectEntry
		for i := range 200 {
			ps = append(ps, projectEntry{Server: defaultServer, Namespace: fmt.Sprintf("org/p%d", i), Path: a.Dir, Description: strings.Repeat("説", 100)})
		}
		data, _ := json.Marshal(projectsFile{Projects: ps})
		writeProjectsFixture(t, a, string(data))
		hints := map[string][]string{projectKey(ps[199]): {"trigger 語 tesla"}, projectKey(ps[198]): {"cwd の .kioku/config"}}
		got := renderProjectList("/bin/kioku", ps, hints, contextLimit)
		if utf8.RuneCountInString(got) > contextLimit || !strings.HasPrefix(got, "- org/p199:") {
			t.Fatal(got)
		}
		lines := strings.Split(got, "\n")
		wantTail := fmt.Sprintf("…ほか %d 件。/bin/kioku session list で全件を見る。", 200-(len(lines)-1))
		if lines[len(lines)-1] != wantTail {
			t.Fatal(lines[len(lines)-1])
		}
		out, stderr, err := runProjectCLI("session", "list")
		if err != nil || stderr != "" || len(strings.Split(strings.TrimSpace(out), "\n")) != 200 {
			t.Fatal(err, stderr, len(out))
		}
	})
}
