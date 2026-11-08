package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func TestPullBindsCWDWithoutLeakingCredentialsOrSession(t *testing.T) {
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("credential not loaded")
		}
		switch r.URL.Path {
		case "/api/projects":
			json.NewEncoder(w).Encode([]model.Project{{ID: "p", Namespace: "org/p"}, {ID: "q", Namespace: "org/q"}})
		default:
			json.NewEncoder(w).Encode(model.SyncManifest{Revision: "empty", Files: map[string]model.SyncFile{}})
		}
	}))
	defer ts.Close()
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".config", "kioku", "auth")
	if err := os.MkdirAll(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIOKU_SERVER", "")
	t.Setenv("KIOKU_AGENT_TOKEN", "")
	t.Setenv("KIOKU_SESSION_ID", "../../unrelated")
	b, _ := json.Marshal(credentials{Server: ts.URL, Token: "secret"})
	os.WriteFile(filepath.Join(authDir, auth.Hash(ts.URL)+".json"), b, 0600)
	run := func(dir string, args ...string) error {
		return Run(context.Background(), append([]string{"-C", dir}, args...), &bytes.Buffer{}, &bytes.Buffer{})
	}
	if err := run(root, "--server", ts.URL, "-n", "org/p", "pull"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".kioku/config"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	json.Unmarshal(b, &saved)
	if len(saved) != 2 || saved["namespace"] != "org/p" || saved["server"] != ts.URL || bytes.Contains(b, []byte("secret")) {
		t.Fatal(string(b))
	}
	for _, name := range []string{"wiki", "maps", ".kioku"} {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil || !info.IsDir() {
			t.Fatal(name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".kioku/manifest.json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := run(root, "pull"); err != nil {
		t.Fatal(err)
	}
	before := requests
	if err := run(root, "--server", ts.URL, "-n", "org/q", "pull"); err == nil {
		t.Fatal("rebound an existing directory")
	}
	if requests != before {
		t.Fatal("conflicting binding issued requests")
	}
	if err := run(root, "--server", "https://other.example", "pull"); err == nil {
		t.Fatal("server override replaced binding")
	}
	child := filepath.Join(root, "child")
	os.Mkdir(child, 0700)
	if err := run(child, "pull"); err == nil || !strings.Contains(err.Error(), "no project configured") {
		t.Fatal("parent config used", err)
	}
	other := t.TempDir()
	if err := run(other, "--server", ts.URL, "-n", "org/q", "pull"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(other, ".kioku/config"))
	if !bytes.Contains(second, []byte("org/q")) {
		t.Fatal(string(second))
	}
}

func TestProjectConfigAndCredentialFilesHaveDifferentScopes(t *testing.T) {
	a := &App{Dir: t.TempDir(), AuthDir: t.TempDir(), Config: Config{Server: "https://one.example", Token: "one"}}
	if err := a.saveCredentials(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(a.credentialPath()); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(err)
	}
	a.Config = Config{Server: "https://two.example"}
	if err := a.loadCredentials(); err != nil || a.Config.Token != "" {
		t.Fatal("another origin's credential loaded", err)
	}
	a.Config = Config{Server: "https://one.example"}
	if err := a.loadCredentials(); err != nil || a.Config.Token != "one" {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.metadataDir()); !os.IsNotExist(err) {
		t.Fatal("auth wrote project state")
	}
}

func TestPullRefusesUnmanagedContentAndUnsafeMetadata(t *testing.T) {
	a, _, _, _ := syncFixture(t)
	os.Mkdir(filepath.Join(a.Dir, "wiki"), 0700)
	path := filepath.Join(a.Dir, "wiki", "private.md")
	os.WriteFile(path, []byte("keep"), 0600)
	if _, err := a.syncWiki(); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep" {
		t.Fatal("unmanaged file removed")
	}
	a, _, _, _ = syncFixture(t)
	outside := t.TempDir()
	os.Symlink(outside, a.metadataDir())
	if _, err := a.syncWiki(); err == nil {
		t.Fatal("symlink metadata accepted")
	}
	if files, _ := os.ReadDir(outside); len(files) != 0 {
		t.Fatal("wrote outside project")
	}
}

func TestBindingCommandsRejectExistingConfigBeforeRemoteMutation(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer ts.Close()
	a := &App{Dir: t.TempDir(), AuthDir: filepath.Join(t.TempDir(), "kioku", "auth"), Config: Config{Server: ts.URL, Namespace: "org/p"}, Client: ts.Client(), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, ctx: context.Background()}
	unlock, err := a.localLock()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveProjectConfig(); err != nil {
		t.Fatal(err)
	}
	unlock()
	a.projectNamespace = "org/other"
	if err := a.createProject(nil); err == nil {
		t.Fatal("create overwrote config")
	}
	if err := a.join([]string{"https://example.test/invite/token"}); err == nil {
		t.Fatal("join overwrote config")
	}
	if calls != 0 {
		t.Fatal("issued remote mutations before local preflight", calls)
	}
}

func TestCreateAndJoinPreflightBeforePOST(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(model.Project{ID: "p", Namespace: "org/p"})
	}))
	defer ts.Close()
	for _, command := range []string{"create", "join"} {
		a := &App{Dir: t.TempDir(), AuthDir: filepath.Join(t.TempDir(), "kioku", "auth"), Config: Config{Server: ts.URL}, Client: ts.Client(), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, ctx: context.Background()}
		if err := os.WriteFile(filepath.Join(a.Dir, "wiki"), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
		a.projectNamespace = "org/p"
		args := []string{"project", command}
		if command == "join" {
			args = append(args, "org/p")
		}
		if err := a.dispatch(args); err == nil {
			t.Fatal("invalid path accepted")
		}
		if calls != 0 {
			t.Fatal("remote mutation before local preflight")
		}
		if _, err := os.Stat(filepath.Join(a.metadataDir(), "config")); !os.IsNotExist(err) {
			t.Fatal("config written despite preflight failure", err)
		}
	}
}

func TestPullIgnoresExistingManifest(t *testing.T) {
	a, _, _, _ := syncFixture(t)
	if _, err := a.syncWiki(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.metadataDir(), "manifest.json")
	if err := os.WriteFile(path, []byte("obsolete invalid metadata"), 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := a.syncWiki(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "obsolete invalid metadata" {
		t.Fatal("existing manifest was modified", string(b), err)
	}
}

func TestBindingRefusesUnmanagedMirrorsOnRetry(t *testing.T) {
	for _, tree := range []string{"wiki", "maps"} {
		t.Run(tree, func(t *testing.T) {
			a, _, _, _ := syncFixture(t)
			if err := os.Mkdir(filepath.Join(a.Dir, tree), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(a.Dir, tree, "private.md")
			if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := a.bindPullProject("org/project"); err == nil {
					t.Fatal("unmanaged files adopted")
				}
				if _, err := os.Stat(filepath.Join(a.metadataDir(), "config")); !os.IsNotExist(err) {
					t.Fatal("failed binding saved config", err)
				}
				if err := a.preflightProjectStorage(); err == nil {
					t.Fatal("remote mutation preflight accepted unmanaged files")
				}
				if b, err := os.ReadFile(path); err != nil || string(b) != "keep" {
					t.Fatal("unmanaged content changed", err)
				}
			}
		})
	}
}

func TestBindingRejectsUnavailableNamespaceWithoutSavingConfig(t *testing.T) {
	a, _, _, _ := syncFixture(t)
	if err := a.bindPullProject("org/not-a-member"); err == nil {
		t.Fatal("unavailable project accepted")
	}
	if _, err := os.Stat(filepath.Join(a.metadataDir(), "config")); !os.IsNotExist(err) {
		t.Fatal("invalid binding persisted", err)
	}
}
