package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func projectRunHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KIOKU_SERVER", "")
	t.Setenv("KIOKU_AGENT_TOKEN", "")
	return home
}

func runProjectCLI(args ...string) (string, string, error) {
	var out, stderr bytes.Buffer
	err := Run(context.Background(), args, &out, &stderr)
	return out.String(), stderr.String(), err
}

func TestProjectFlagPullAndDescriptionFromAnyCWD(t *testing.T) {
	home := projectRunHome(t)
	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/projects" {
			fmt.Fprint(w, `[{"namespace":"org/p"}]`)
			return
		}
		fmt.Fprint(w, `{"revision":"empty","files":{}}`)
	}))
	defer server.Close()
	prefix := []string{"-n", "org/p", "--server", server.URL}
	if _, stderr, err := runProjectCLI(append(prefix, "pull", "--description", "first description")...); err != nil || stderr != "" {
		t.Fatal(err, stderr)
	}
	project := filepath.Join(home, ".local", "share", "kioku", "projects", "org", "p")
	path := (&App{AuthDir: filepath.Join(home, ".config", "kioku", "auth")}).projectsPath()
	assertEntry := func(description string) {
		t.Helper()
		p, err := readProjects(path)
		if err != nil || len(p.Projects) != 1 || p.Projects[0].Path != project || p.Projects[0].Description != description {
			t.Fatal(p, err)
		}
	}
	assertEntry("first description")
	if info, err := os.Stat(project); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal(info, err)
	}
	t.Chdir(t.TempDir())
	if _, _, err := runProjectCLI(append(prefix, "status")...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runProjectCLI(append(prefix, "pull", "--description=changed")...); err != nil {
		t.Fatal(err)
	}
	assertEntry("changed")
	if _, _, err := runProjectCLI(append(prefix, "pull")...); err != nil {
		t.Fatal(err)
	}
	assertEntry("changed")
	if _, _, err := runProjectCLI(append(prefix, "pull", "--description", "")...); err != nil {
		t.Fatal(err)
	}
	assertEntry("")
	if err := os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runProjectCLI(append(prefix, "pull")...); err != nil {
		t.Fatal("deleted directory did not rebind", err)
	}
	assertEntry("")
}

func TestProjectFlagServerPrecedenceAndRegisteredPath(t *testing.T) {
	for _, source := range []string{"default", "environment", "flag"} {
		t.Run(source, func(t *testing.T) {
			home := projectRunHome(t)
			a := &App{AuthDir: filepath.Join(home, ".config", "kioku", "auth")}
			servers := []string{defaultServer, "https://env.example", "https://flag.example"}
			paths := []string{filepath.Join(home, "default"), filepath.Join(home, "environment"), filepath.Join(home, "flag")}
			for i, server := range servers {
				a.Config = Config{Server: server, Namespace: "org/p"}
				a.Dir = paths[i]
				if err := a.addProjectEntry(a.Config.Server, a.Config.Namespace, a.Dir, nil); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"-n", "org/p"}
			selected := 0
			if source != "default" {
				t.Setenv("KIOKU_SERVER", servers[1])
				selected = 1
			}
			if source == "flag" {
				args = append(args, "--server", "https://FLAG.example:443/")
				selected = 2
			}
			_, _, err := runProjectCLI(append(args, "status")...)
			if err == nil || !strings.Contains(err.Error(), "kioku -n org/p pull first") {
				t.Fatal(err)
			}
			for i, path := range paths {
				_, err := os.Stat(path)
				if i == selected && err != nil || i != selected && !os.IsNotExist(err) {
					t.Fatal("wrong path selected", path, err)
				}
			}
		})
	}
}

func TestProjectFlagRejectsConflictsWithoutRequests(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		binding  Config
		contents string
		want     string
	}{
		{name: "join", args: []string{"project", "join", "invite"}, want: "project join"},
		{name: "extra positional argument is a usage error", args: []string{"pull", ""}, want: "usage:"},
		{name: "create mismatch", args: []string{"project", "create", "org/q"}, want: "usage:"},
		{name: "pull mismatch", args: []string{"pull", "org/q"}, want: "usage:"},
		{name: "explicit namespace", args: []string{"-C", ".", "pull"}, binding: Config{Namespace: "org/q"}, want: "conflicts"},
		{name: "explicit server", args: []string{"-C", ".", "pull"}, binding: Config{Server: "https://other.example", Namespace: "org/p"}, want: "conflicts"},
		{name: "namespace", args: []string{"pull"}, binding: Config{Namespace: "org/q"}, want: "conflicts"},
		{name: "server", args: []string{"whoami"}, binding: Config{Server: "https://other.example", Namespace: "org/p"}, want: "conflicts"},
		{name: "broken JSON", args: []string{"pull"}, contents: "{", want: "projects.json"},
		{name: "relative path", args: []string{"pull"}, contents: `{"projects":[{"path":"relative"}]}`, want: "projects.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := projectRunHome(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(400) }))
			defer server.Close()
			project := filepath.Join(home, ".local", "share", "kioku", "projects", "org", "p")
			configPath := filepath.Join(project, ".kioku", "config")
			var before []byte
			if test.binding.Namespace != "" {
				binding := test.binding
				if binding.Server == "" {
					binding.Server = server.URL
				}
				os.MkdirAll(filepath.Dir(configPath), 0700)
				before, _ = json.Marshal(binding)
				os.WriteFile(configPath, before, 0600)
			}
			if strings.HasPrefix(test.name, "explicit ") {
				t.Chdir(project)
			}
			if test.contents != "" {
				writeProjectsFixture(t, &App{AuthDir: filepath.Join(home, ".config", "kioku", "auth")}, test.contents)
			}
			args := append([]string{"--server", server.URL, "-n", "org/p"}, test.args...)
			_, _, err := runProjectCLI(args...)
			if err == nil || !strings.Contains(err.Error(), test.want) || calls != 0 {
				t.Fatal(err, calls)
			}
			after, _ := os.ReadFile(configPath)
			if !bytes.Equal(before, after) {
				t.Fatal("config rewritten")
			}
		})
	}
	for _, namespace := range []string{"", "org", "../p", "org/../p", "org/p/extra"} {
		if _, _, err := runProjectCLI("-n", namespace, "pull"); err == nil {
			t.Fatal("invalid namespace accepted", namespace)
		}
	}
}

func TestProjectFlagUnboundCommandBoundary(t *testing.T) {
	home := projectRunHome(t)
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if r.URL.Path == "/api/auth/loopback" {
			w.WriteHeader(400)
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	a := &App{AuthDir: filepath.Join(home, ".config", "kioku", "auth"), Config: Config{Server: server.URL, Token: "human"}}
	if err := a.saveCredentials(); err != nil {
		t.Fatal(err)
	}
	seedTrustedOrigin(t, a.credentialPath(), server.URL)
	prefix := []string{"--server", server.URL, "-n", "org/p"}
	for _, args := range [][]string{{"status"}, {"put"}, {"generate"}, {"project", "list"}, {"project", "delete", "--yes"}, {"agent", "list"}, {"org", "list"}} {
		_, _, err := runProjectCLI(append(prefix, args...)...)
		if err == nil || !strings.Contains(err.Error(), "kioku -n org/p pull first") {
			t.Fatal(args, err)
		}
	}
	if len(calls) != 0 {
		t.Fatal("unbound command reached server", calls)
	}
	if _, _, err := runProjectCLI(append(prefix, "whoami")...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runProjectCLI(append(prefix, "login")...); err == nil {
		t.Fatal("login should return mock server's rejection")
	}
	if _, _, err := runProjectCLI(append(prefix, "logout")...); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "/api/me,/api/auth/loopback,/api/logout" {
		t.Fatal(calls)
	}
	config := filepath.Join(home, ".local", "share", "kioku", "projects", "org", "p", ".kioku", "config")
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Fatal("authentication bound project", err)
	}
}

func TestProjectFlagCreateAndExplicitDirectoryDescriptions(t *testing.T) {
	for _, command := range []string{"create", "join", "pull"} {
		t.Run(command, func(t *testing.T) {
			home := projectRunHome(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					if command == "create" {
						var body map[string]string
						json.NewDecoder(r.Body).Decode(&body)
						if body["name"] != "org/p" {
							t.Error(body)
						}
					}
					fmt.Fprint(w, `{"namespace":"org/p"}`)
					return
				}
				if r.URL.Path == "/api/projects" {
					fmt.Fprint(w, `[{"namespace":"org/p"}]`)
					return
				}
				fmt.Fprint(w, `{"revision":"empty","files":{}}`)
			}))
			defer server.Close()
			args := []string{"--server", server.URL}
			var project string
			if command == "create" {
				args = append(args, "-n", "org/p", "project", "create", "--description", "created")
				project = filepath.Join(home, ".local", "share", "kioku", "projects", "org", "p")
			} else {
				project = t.TempDir()
				args = append(args, "-C", project)
				if command == "join" {
					args = append(args, "project")
				}
				if command == "join" {
					args = append(args, command, "--description", "created", "org/p")
				} else {
					args = append(args, "-n", "org/p", command, "--description", "created")
				}
			}
			if _, _, err := runProjectCLI(args...); err != nil {
				t.Fatal(err)
			}
			p, err := readProjects((&App{AuthDir: filepath.Join(home, ".config", "kioku", "auth")}).projectsPath())
			if err != nil || len(p.Projects) != 1 || p.Projects[0].Description != "created" || p.Projects[0].Path != project {
				t.Fatal(p, err)
			}
		})
	}
}

func TestProjectFlagAgentLoginPinsRequestedNamespace(t *testing.T) {
	for _, issuedNamespace := range []string{"org/p", "org/q"} {
		t.Run(issuedNamespace, func(t *testing.T) {
			home := projectRunHome(t)
			revoked := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/logout" {
					revoked = true
					fmt.Fprint(w, `{}`)
					return
				}
				var in map[string]string
				json.NewDecoder(r.Body).Decode(&in)
				if in["namespace"] != "org/p" {
					t.Error("requested namespace missing", in)
				}
				json.NewEncoder(w).Encode(agentCredentials{ID: "agent", Token: "kioku_agent.agent.secret", Role: "read", Expires: time.Now().Add(time.Hour), Namespace: issuedNamespace})
			}))
			defer server.Close()
			a := &App{AuthDir: filepath.Join(home, ".config", "kioku", "auth"), Config: Config{Server: server.URL}}
			seedTrustedOrigin(t, a.credentialPath(), server.URL)
			_, _, err := runProjectCLI("--server", server.URL, "-n", "org/p", "agent", "login", "kioku_code.test")
			configPath := filepath.Join(home, ".local", "share", "kioku", "projects", "org", "p", ".kioku", "config")
			if issuedNamespace == "org/p" {
				if err != nil || revoked {
					t.Fatal(err, revoked)
				}
				if _, err := os.Stat(configPath); err != nil {
					t.Fatal(err)
				}
				p, err := readProjects((&App{AuthDir: filepath.Join(home, ".config", "kioku", "auth")}).projectsPath())
				if err != nil || len(p.Projects) != 1 {
					t.Fatal(p, err)
				}
			} else {
				if err == nil || !revoked {
					t.Fatal("mismatched namespace accepted", err, revoked)
				}
				if _, err := os.Stat(configPath); !os.IsNotExist(err) {
					t.Fatal("mismatch wrote config", err)
				}
				if _, err := os.Stat((&App{AuthDir: filepath.Join(home, ".config", "kioku", "auth")}).projectsPath()); !os.IsNotExist(err) {
					t.Fatal("mismatch registered project", err)
				}
			}
		})
	}
}

func TestProjectFlagPreservesUnboundMirror(t *testing.T) {
	home := projectRunHome(t)
	project := filepath.Join(home, ".local", "share", "kioku", "projects", "org", "p")
	os.MkdirAll(filepath.Join(project, "wiki"), 0700)
	path := filepath.Join(project, "wiki", "mine.md")
	os.WriteFile(path, []byte("keep"), 0600)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `[{"namespace":"org/p"}]`)
	}))
	defer server.Close()
	if _, _, err := runProjectCLI("--server", server.URL, "-n", "org/p", "pull"); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep" {
		t.Fatal("unbound mirror changed")
	}
	if calls != 0 {
		t.Fatal("unbound mirror reached server", calls)
	}
}

func TestProjectHelpDescribesSelectionAndDescriptions(t *testing.T) {
	out, _, err := runProjectCLI("help")
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"-n ORG/PROJECT", "--description TEXT", "bind cwd (or -C)", "projects.json", "create requires -n", "-C with -n", "flags must precede positional arguments"} {
		if !strings.Contains(out, phrase) {
			t.Fatal("missing help text", phrase)
		}
	}
}

func TestProjectFlagsRejectInvalidArguments(t *testing.T) {
	for _, command := range [][]string{{"project", "create"}, {"project", "join"}, {"pull"}} {
		for _, args := range [][]string{{"--description"}, {"--unknown"}, {"org/p", "--description", "text"}, {"org/p", "org/q"}} {
			a := &App{Out: io.Discard, Err: io.Discard}
			if err := a.dispatch(append(append([]string{}, command...), args...)); err == nil {
				t.Fatal("accepted invalid arguments", command, args)
			}
		}
	}
}

func TestProjectFlagSelectsDirectory(t *testing.T) {
	for _, command := range []string{"pull", "create"} {
		for _, selection := range []string{"default", "registered", "explicit"} {
			t.Run(command+"/"+selection, func(t *testing.T) {
				home := projectRunHome(t)
				cwd := t.TempDir()
				t.Chdir(cwd)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "POST" {
						var body map[string]string
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["name"] != "org/p" {
							t.Errorf("create body: %v, %v", body, err)
						}
						fmt.Fprint(w, `{"namespace":"org/p"}`)
					} else if r.URL.Path == "/api/projects" {
						fmt.Fprint(w, `[{"namespace":"org/p"}]`)
					} else {
						fmt.Fprint(w, `{"revision":"empty","files":{}}`)
					}
				}))
				defer server.Close()
				a := &App{AuthDir: filepath.Join(home, ".config", "kioku", "auth")}
				// The same namespace on another server must not select this path.
				otherPath := filepath.Join(home, "other-server")
				if err := a.addProjectEntry("https://other.example", "org/p", otherPath, nil); err != nil {
					t.Fatal(err)
				}
				selected := filepath.Join(home, ".local", "share", "kioku", "projects", "org", "p")
				registered := filepath.Join(home, "registered")
				if selection != "default" {
					if err := a.addProjectEntry(server.URL, "org/p", registered, nil); err != nil {
						t.Fatal(err)
					}
					selected = registered
				}
				args := []string{"--server", server.URL, "-n", "org/p"}
				if selection == "explicit" {
					selected = t.TempDir()
					args = append(args, "-C", selected)
				}
				if command == "create" {
					args = append(args, "project")
				}
				args = append(args, command, "--description", "selected project")
				if _, _, err := runProjectCLI(args...); err != nil {
					t.Fatal(err)
				}
				if command == "pull" && selection == "explicit" {
					if _, _, err := runProjectCLI(args...); err != nil {
						t.Fatal("matching binding rejected", err)
					}
				}
				bound, err := (&App{Dir: selected}).readProjectConfig()
				if err != nil || bound.Server != server.URL || bound.Namespace != "org/p" {
					t.Fatal(bound, err)
				}
				if selection != "explicit" {
					if info, err := os.Stat(selected); err != nil || info.Mode().Perm() != 0700 {
						t.Fatal(info, err)
					}
				}
				for _, untouched := range []string{cwd, otherPath} {
					if _, err := os.Stat(filepath.Join(untouched, ".kioku")); !os.IsNotExist(err) {
						t.Fatal("unexpected binding", untouched, err)
					}
				}
				if selection == "explicit" {
					if _, err := os.Stat(registered); !os.IsNotExist(err) {
						t.Fatal("-C did not override registered path", err)
					}
				}
				projects, err := readProjects(a.projectsPath())
				if err != nil {
					t.Fatal(err)
				}
				i := projects.find(server.URL, "org/p")
				wantPath := selected
				if selection == "explicit" {
					wantPath = registered
				}
				if i < 0 || projects.Projects[i].Path != wantPath || projects.Projects[i].Description != "selected project" {
					t.Fatal(projects)
				}
			})
		}
	}
}

func TestProjectSelectionUsageErrors(t *testing.T) {
	projectRunHome(t)
	t.Chdir(t.TempDir())
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"pull"}, "no project configured"},
		{[]string{"project", "create"}, "usage: kioku -n ORG/PROJECT project create"},
		{[]string{"project", "create", "org/p"}, "usage: kioku -n ORG/PROJECT project create"},
		{[]string{"-n", "org/p", "pull", "org/q"}, "usage: kioku [-n ORG/PROJECT] pull"},
		{[]string{"pull", "org/p"}, "usage: kioku [-n ORG/PROJECT] pull"},
		{[]string{"pull", "org/p", "--description", "text"}, "usage: kioku [-n ORG/PROJECT] pull"},
		{[]string{"project", "create", "org/p", "--description", "text"}, "usage: kioku -n ORG/PROJECT project create"},
		{[]string{"pull", "--unknown", "org/p"}, "flag provided but not defined"},
		{[]string{"project", "create", "--description"}, "flag needs an argument"},
	} {
		_, _, err := runProjectCLI(append([]string{"--server", server.URL}, test.args...)...)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%v: %v; want %s", test.args, err, test.want)
		}
	}
	if calls != 0 {
		t.Fatal("invalid arguments reached server", calls)
	}
}

func TestProjectFlagPullBindsOnlyAfterManifest(t *testing.T) {
	for _, test := range []struct {
		name       string
		listed     bool
		status     int
		body, want string
		bound      bool
	}{
		{name: "not listed", want: "project org/p not found, or you are not a member"},
		{name: "forbidden", listed: true, status: 403, want: "project access lost"},
		{name: "not found", listed: true, status: 404, want: "project org/p not found, or you are not a member"},
		{name: "unauthorized", listed: true, status: 401, body: `{"error":"login required"}`, want: "login required"},
		{name: "upgrade required", listed: true, status: 426, body: `{"error":"upgrade kioku to 0.1.5 or later"}`, want: "upgrade kioku to 0.1.5 or later"},
		{name: "invalid manifest", listed: true, status: 200, body: `{}`, want: "invalid sync manifest"},
		{name: "success", listed: true, status: 200, body: `{"revision":"empty","files":{}}`, bound: true},
		{name: "file failure", listed: true, status: 200, body: `{"revision":"r","files":{"wiki/index.md":{"bytes":1,"hash":"` + strings.Repeat("0", 64) + `"}}}`, want: "wiki/index.md: file unavailable", bound: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := projectRunHome(t)
			project := filepath.Join(home, ".local", "share", "kioku", "projects", "org", "p")
			configPath := filepath.Join(project, ".kioku", "config")
			projectsPath := (&App{AuthDir: filepath.Join(home, ".config", "kioku", "auth")}).projectsPath()
			assertBound := func(want bool) {
				t.Helper()
				for _, path := range []string{configPath, projectsPath} {
					_, err := os.Stat(path)
					if want && err != nil || !want && !os.IsNotExist(err) {
						t.Errorf("binding %s: err=%v, want bound=%v", path, err, want)
					}
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/projects" {
					assertBound(false)
					if test.listed {
						fmt.Fprint(w, `[{"namespace":"org/p"}]`)
					} else {
						fmt.Fprint(w, `[]`)
					}
					return
				}
				if !test.listed || r.URL.Path != "/api/projects/org/p/snapshot" {
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(400)
					return
				}
				if r.URL.Query().Get("file") != "" {
					assertBound(true)
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":"file unavailable"}`)
					return
				}
				assertBound(false)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			_, _, err := runProjectCLI("--server", server.URL, "-n", "org/p", "pull")
			if test.want == "" && err != nil || test.want != "" && (err == nil || err.Error() != test.want) {
				t.Errorf("pull error: %v, want %q", err, test.want)
			}
			assertBound(test.bound)
		})
	}
}

func TestSubcommandHelpSucceeds(t *testing.T) {
	projectRunHome(t)
	t.Chdir(t.TempDir())
	for _, command := range [][]string{{"project", "create"}, {"project", "join"}, {"pull"}, {"put"}, {"generate"}} {
		for _, flag := range []string{"-h", "--help"} {
			t.Run(strings.Join(command, " ")+"/"+flag, func(t *testing.T) {
				_, stderr, err := runProjectCLI(append(append([]string{}, command...), flag)...)
				if err != nil || !strings.Contains(stderr, "Usage of ") || strings.Contains(stderr, "flag: help requested") {
					t.Fatalf("help: err=%v, stderr=%q", err, stderr)
				}
			})
		}
	}
}
