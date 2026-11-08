package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/cli"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/server"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/prompts"
)

func TestCLIFullWorkflow(t *testing.T) {
	data := t.TempDir()
	db, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	key, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := prompts.Load("", prompts.Variables{ClientVersion: server.MinimumClientVersion, MaxOutputBytes: 128 << 20}, server.MCPToolNames())
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(server.Config{Prompts: rendered, BaseURL: "http://127.0.0.1"}, db, local.Store{Dir: filepath.Join(data, "objects")}, key)
	g, err := runners.Load(runners.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetRunners(g)
	ts := httptest.NewServer(s.Handler())
	s.Config.BaseURL = ts.URL
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.RunWorker(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		ts.Close()
		if err := g.Close(context.Background()); err != nil {
			t.Error(err)
		}
		db.Close()
	}()
	config := t.TempDir()
	run := func(args ...string) (string, error) {
		t.Helper()
		var out, stderr bytes.Buffer
		all := append([]string{"-C", config, "--server", ts.URL}, args...)
		err := cli.Run(ctx, all, &out, &stderr)
		return out.String(), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	// Authentication is seeded only in this test, never through a server route.
	token := auth.Token()
	if err = db.Update(func(st *model.State) error {
		st.Users["test-user"] = model.User{ID: "test-user", Name: "Alice", Email: "alice@example.com", Identities: map[string]string{"test": "alice"}}
		st.Sessions[auth.Hash(token)] = model.Session{UserID: "test-user", Expires: time.Now().Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	authDir := filepath.Join(home, ".config", "kioku", "auth")
	if err := os.MkdirAll(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	initial, _ := json.Marshal(map[string]string{"server": ts.URL, "token": token})
	if err = os.WriteFile(filepath.Join(authDir, auth.Hash(ts.URL)+".json"), initial, 0600); err != nil {
		t.Fatal(err)
	}
	must("whoami")
	must("project", "create", "Test project")
	must("project", "share", "list")
	must("project", "share", "invite", "bob@example.com")
	b, err := os.ReadFile(filepath.Join(config, ".kioku", "config"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg cli.Config
	if err = json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	project := config
	if err = os.WriteFile(filepath.Join(project, "inbox", "notes.md"), []byte(cli.SourceContentForTest("https://example.com/notes", "# Architecture\nAll application code is Go. Objects live in S3.")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := run("put", "--source-url", "https://example.com/override", filepath.Join(project, "inbox", "notes.md")); err == nil {
		t.Fatal("accepted removed source-url flag")
	}
	must("put")
	if out := must("status"); !strings.Contains(out, `"inbox_count": 1`) {
		t.Fatal(out)
	}
	if out := must("put"); !strings.Contains(out, "新しい投入ファイルはありません") {
		t.Fatal(out)
	}
	must("logout")
	if _, err = run("whoami"); err == nil {
		t.Fatal("logout did not revoke session")
	}
}
