package server

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/store"
)

func objectFixture(t *testing.T, dir string, objects blob.Store) *fixture {
	t.Helper()
	stateStore, err := openFixtureDatabase(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Prompts: defaultPrompts(t)}, stateStore, objects, key)
	g, err := runners.Load(runners.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetRunners(g)
	ts := httptest.NewServer(s.Handler())
	s.Config.BaseURL = ts.URL
	t.Cleanup(func() { ts.Close(); stateStore.Close() })
	return &fixture{t: t, s: s, url: ts.URL, client: ts.Client(), stateStore: stateStore}
}
func runGeneration(t *testing.T, f *fixture, token, pid string) fixtureJob {
	t.Helper()
	if f.auto == nil {
		configureAutoRunner(t, f)
	}
	j := startManaged(t, f, token, pid, false)
	if j.Unchanged {
		return j
	}
	sendOutput(t, f, f.auto, j, "Generated")
	f.auto.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	return reconcileManaged(t, f, j)
}

func TestRestoringSQLiteSelectsBackupContentReferences(t *testing.T) {
	root, remote := t.TempDir(), t.TempDir()
	objects := local.Store{Dir: remote}
	f := objectFixture(t, root, objects)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	base := "/api/projects/" + p.ID
	putFile(t, f, token, p.ID, "first.md", "First source")
	var projects []fixtureProjectEntry
	if c := f.request("GET", "/api/projects", token, nil, &projects); c != 200 || len(projects) != 1 || projects[0].InboxCount != 1 {
		t.Fatal(c, projects)
	}
	runGeneration(t, f, token, p.ID)
	backup := filepath.Join(t.TempDir(), "backup")
	if err := f.stateStore.Backup(backup); err != nil {
		t.Fatal(err)
	}
	putFile(t, f, token, p.ID, "second.md", "Newer than SQLite backup")
	runGeneration(t, f, token, p.ID)
	putFile(t, f, token, p.ID, "third.md", "Still waiting")
	restored := objectFixture(t, backup, objects)
	all := ""
	for _, body := range restored.readWiki(base, token) {
		all += body
	}
	if strings.Contains(all, "Newer than SQLite backup") || !strings.Contains(all, "First source") {
		t.Fatal("backup references were not restored")
	}
	_ = restored.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if len(fixtureRevisionsList(t, st)) != 1 || len(fixtureInboxList(t, st)) != 0 {
			t.Error("project content lost during restore", fixtureInboxList(t, st))
		}
		return nil
	})
	keys, err := objects.List(context.Background(), "projects/"+p.ID+"/")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if strings.Contains(string(key), "/revisions/") {
			t.Error("full wiki history retained", key)
		}
	}
	if _, err = os.Stat(filepath.Join(backup, "projects")); !os.IsNotExist(err) {
		t.Fatal("content databases included in config backup")
	}
}

type failCurrentWiki struct {
	blob.Store
	fail bool
}

func (b *failCurrentWiki) Put(ctx context.Context, key blob.Key, body []byte) error {
	if b.fail && strings.Contains(string(key), "/.kioku/wiki/") && strings.HasPrefix(string(body), "# Log\n") {
		return errors.New("temporary S3 upload failure")
	}
	return b.Store.Put(ctx, key, body)
}
func TestInterruptedImmutableWikiCanBeRetried(t *testing.T) {
	objects := &failCurrentWiki{Store: local.Store{Dir: t.TempDir()}, fail: true}
	f := objectFixture(t, t.TempDir(), objects)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	configureAutoRunner(t, f)
	putFile(t, f, token, p.ID, "note.md", "Durable input")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, f.auto, j, "Generated")
	f.auto.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	deferredPublication(t, f, j)
	if p := fixtureProjectsGet(t, f, p.ID); !p.WikiIncomplete {
		t.Fatal(p)
	}
	if f.readWiki("/api/projects/"+p.ID, token)["wiki/index.md"] == "" {
		t.Fatal("partial progress unavailable")
	}
	objects.fail = false
	if got := reconcileManaged(t, f, j); got.Status != "completed" {
		t.Fatal(got)
	}
	if project := fixtureProjectsGet(t, f, p.ID); project.WikiIncomplete || len(fixtureInboxList(t, f)) != 0 {
		t.Fatal(project)
	}
	if _, err := objects.Get(t.Context(), blob.Key("projects/"+p.ID+"/.kioku/state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("metadata object exists", err)
	}
}
