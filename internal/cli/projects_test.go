package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func projectEntryApp(t *testing.T) *App {
	t.Helper()
	return &App{Dir: t.TempDir(), AuthDir: filepath.Join(t.TempDir(), "kioku", "auth"), Config: Config{Server: defaultServer, Namespace: "org/p"}, Out: io.Discard, Err: &bytes.Buffer{}}
}

func writeProjectsFixture(t *testing.T, a *App, contents string) string {
	t.Helper()
	path := a.projectsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectsNoOpDescriptionAndDelete(t *testing.T) {
	a := projectEntryApp(t)
	path := writeProjectsFixture(t, a, `{"projects":[{"path":"/original","namespace":"org/p","server":"https://kioku.magicians.ee","description":"old"},{"path":"/other","namespace":"org/q","server":"https://kioku.magicians.ee"}]}`)
	before, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if err := a.addProjectEntry(a.Config.Server, a.Config.Namespace, a.Dir, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	newInfo, _ := os.Stat(path)
	if !bytes.Equal(before, after) || !os.SameFile(info, newInfo) {
		t.Fatal("existing entry caused a write")
	}
	if !strings.Contains(a.Err.(*bytes.Buffer).String(), "/original") {
		t.Fatal("missing existing path warning")
	}
	description := "new description"
	if err := a.addProjectEntry(a.Config.Server, a.Config.Namespace, a.Dir, &description); err != nil {
		t.Fatal(err)
	}
	p, err := readProjects(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Projects[0]; got.Path != "/original" || got.Description != description {
		t.Fatalf("entry changed unexpectedly: %+v", got)
	}
	if err := a.deleteProjectEntry(); err != nil {
		t.Fatal(err)
	}
	p, err = readProjects(path)
	if err != nil || len(p.Projects) != 1 || p.Projects[0].Namespace != "org/q" {
		t.Fatal(p, err)
	}
}

func TestProjectsDeleteMissingFileDoesNotCreateIt(t *testing.T) {
	for _, directoryExists := range []bool{false, true} {
		a := projectEntryApp(t)
		path := a.projectsPath()
		if directoryExists {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.deleteProjectEntry(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("delete created projects.json", err)
		}
	}
}

func TestProjectsRejectInvalidInputWithoutRewriting(t *testing.T) {
	for _, contents := range []string{
		"",
		"{",
		`[]`,
		`{"projects":{}}`,
		`{"projects":[null]}`,
		`{"projects":[{"server":"https://example.com","namespace":"org/p","path":"relative"}]}`,
		`{"projects":[{"server":"https://example.com","namespace":"org/p","path":2}]}`,
		`{"projects":[{"server":2,"namespace":"org/p","path":"/project"}]}`,
	} {
		t.Run(contents, func(t *testing.T) {
			a := projectEntryApp(t)
			path := writeProjectsFixture(t, a, contents)
			for _, update := range []func() error{func() error { return a.addProjectEntry(a.Config.Server, a.Config.Namespace, a.Dir, nil) }, a.deleteProjectEntry} {
				err := a.projectEntryResult(update(), "retry after repair")
				if err == nil || !strings.Contains(err.Error(), path) {
					t.Fatal("invalid file did not stop command", err)
				}
				b, _ := os.ReadFile(path)
				if string(b) != contents {
					t.Fatal("rewrote invalid file")
				}
			}
		})
	}
}

func TestProjectsConcurrentRegistrationAndPermissions(t *testing.T) {
	a := projectEntryApp(t)
	const count = 24
	errors := make(chan error, count)
	for i := range count {
		go func(i int) {
			copy := *a
			copy.Err = io.Discard
			copy.Config.Namespace = fmt.Sprintf("org/p%d", i)
			errors <- copy.addProjectEntry(copy.Config.Server, copy.Config.Namespace, copy.Dir, nil)
		}(i)
	}
	for range count {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	path := a.projectsPath()
	p, err := readProjects(path)
	if err != nil || len(p.Projects) != count {
		t.Fatal("lost a concurrent update", p, err)
	}
	for i := range count {
		if p.find(defaultServer, fmt.Sprintf("org/p%d", i)) < 0 {
			t.Fatal("missing entry", i)
		}
	}
	for path, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("permissions", path, info, err)
		}
	}
}

func TestProjectConfigRegistersAndPullRepairsEntry(t *testing.T) {
	a, _, _, _ := syncFixture(t)
	a.AuthDir = filepath.Join(t.TempDir(), "kioku", "auth")
	if err := a.bindPullProject("org/project"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(a.metadataDir(), "config"), a.projectsPath()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("binding saved config before sync", path, err)
		}
	}
	if err := a.pull(); err != nil {
		t.Fatal(err)
	}
	path := a.projectsPath()
	p, err := readProjects(path)
	if err != nil || p.find(a.Config.Server, a.Config.Namespace) < 0 {
		t.Fatal(p, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := a.pull(); err != nil {
		t.Fatal(err)
	}
	p, err = readProjects(path)
	if err != nil || p.find(a.Config.Server, a.Config.Namespace) < 0 {
		t.Fatal("pull did not repair entry", p, err)
	}
}

func TestProjectEntryWriteFailureIsOnlyWarning(t *testing.T) {
	a, _, _, _ := syncFixture(t)
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	a.AuthDir = filepath.Join(blocked, "auth")
	if err := a.pull(); err != nil {
		t.Fatal("registration failure failed pull", err)
	}
	if got := a.Err.(*bytes.Buffer).String(); !strings.Contains(got, "projects.json") || !strings.Contains(got, "next pull") {
		t.Fatal(got)
	}
	if config, err := a.readProjectConfig(); err != nil || config.Namespace != a.Config.Namespace {
		t.Fatal(config, err)
	}
}

func TestProjectCreateJoinAndDeleteUpdateProjects(t *testing.T) {
	for _, command := range []string{"create", "join"} {
		t.Run(command, func(t *testing.T) {
			a := projectEntryApp(t)
			a.ctx = context.Background()
			deleteStatus := http.StatusForbidden
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "DELETE" {
					w.WriteHeader(deleteStatus)
					fmt.Fprint(w, `{}`)
					return
				}
				fmt.Fprint(w, `{"namespace":"org/p"}`)
			}))
			defer ts.Close()
			a.Client, a.Config.Server, a.Config.Namespace = ts.Client(), ts.URL, ""
			if command == "create" {
				a.projectNamespace = "org/p"
			}
			args := []string{"project", command}
			if command == "join" {
				args = append(args, "org/p")
			}
			if err := a.dispatch(args); err != nil {
				t.Fatal(err)
			}
			path := a.projectsPath()
			p, err := readProjects(path)
			if err != nil || p.find(ts.URL, "org/p") < 0 {
				t.Fatal(p, err)
			}
			if err := a.deleteProject([]string{"--yes"}); err == nil {
				t.Fatal("failed delete succeeded")
			}
			p, _ = readProjects(path)
			if len(p.Projects) != 1 {
				t.Fatal("failed delete removed entry")
			}
			deleteStatus = http.StatusOK
			if err := a.deleteProject([]string{"--yes"}); err != nil {
				t.Fatal(err)
			}
			p, err = readProjects(path)
			if err != nil || len(p.Projects) != 0 {
				t.Fatal(p, err)
			}
		})
	}
}

func TestProjectDeleteWriteFailureWarnsWithoutPullAdvice(t *testing.T) {
	a := projectEntryApp(t)
	a.ctx = context.Background()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Error("unexpected request", r.Method)
		}
		calls++
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	a.Client, a.Config.Server = server.Client(), server.URL
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	a.AuthDir = filepath.Join(blocked, "auth")
	if err := a.deleteProject([]string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	warning := a.Err.(*bytes.Buffer).String()
	if calls != 1 || !strings.Contains(warning, "projects.json") || !strings.Contains(warning, "remove") || strings.Contains(warning, "next pull") {
		t.Fatal(calls, warning)
	}
}
