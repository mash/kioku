package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/store"
)

func objectFixture(t *testing.T, dir string, objects blob.Store) *fixture {
	t.Helper()
	db, err := store.OpenWithObjects(dir, objects)
	if err != nil {
		t.Fatal(err)
	}
	key, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{}, db, objects, key)
	g, err := runners.Load(runners.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetRunners(g)
	ts := httptest.NewServer(s.Handler())
	s.Config.BaseURL = ts.URL
	t.Cleanup(func() { ts.Close(); db.Close() })
	return &fixture{t: t, s: s, url: ts.URL, client: ts.Client(), db: db}
}
func runGeneration(t *testing.T, f *fixture, token, pid string) model.Job {
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

func TestRestoringSQLiteDoesNotRollBackProjectContent(t *testing.T) {
	root, remote := t.TempDir(), t.TempDir()
	objects := local.Store{Dir: remote}
	f := objectFixture(t, root, objects)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	base := "/api/projects/" + p.ID
	putFile(t, f, token, p.ID, "first.md", "First source")
	var projects []model.ProjectEntry
	if c := f.request("GET", "/api/projects", token, nil, &projects); c != 200 || len(projects) != 1 || projects[0].InboxCount != 1 {
		t.Fatal(c, projects)
	}
	runGeneration(t, f, token, p.ID)
	backup := filepath.Join(t.TempDir(), "backup")
	if err := f.db.Backup(backup); err != nil {
		t.Fatal(err)
	}
	putFile(t, f, token, p.ID, "second.md", "Newer than SQLite backup")
	runGeneration(t, f, token, p.ID)
	waiting := putFile(t, f, token, p.ID, "third.md", "Still waiting")
	restored := objectFixture(t, backup, objects)
	var snapshot model.Snapshot
	if c := restored.request("GET", base+"/snapshot", token, nil, &snapshot); c != 200 {
		t.Fatal(c)
	}
	all := ""
	for _, body := range snapshot.Files {
		all += body
	}
	if !strings.Contains(all, "Newer than SQLite backup") {
		t.Fatal("old SQLite backup rolled back wiki")
	}
	_ = restored.db.View(func(st model.State) error {
		if len(st.Revisions) != 1 || len(st.Inbox) != 1 || st.Inbox[waiting.ID].Status != "fetched" {
			t.Error("project content lost during restore", st.Inbox)
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
	if b.fail && strings.HasSuffix(string(key), "/wiki/index.md") {
		return errors.New("temporary S3 upload failure")
	}
	return b.Store.Put(ctx, key, body)
}
func TestInterruptedMutableWikiCanBeRetried(t *testing.T) {
	objects := &failCurrentWiki{Store: local.Store{Dir: t.TempDir()}, fail: true}
	f := objectFixture(t, t.TempDir(), objects)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	putFile(t, f, token, p.ID, "note.md", "Durable input")
	base := "/api/projects/" + p.ID
	failed := runGeneration(t, f, token, p.ID)
	if failed.Status != "failed" {
		t.Fatal(failed)
	}
	_ = f.db.View(func(st model.State) error {
		p := st.Projects[p.ID]
		if !p.WikiIncomplete || len(st.Inbox) != 1 {
			t.Error("partial wiki marked complete", p)
		}
		return nil
	})
	var partial model.Snapshot
	if c := f.request("GET", base+"/snapshot", token, nil, &partial); c != 200 || partial.Files["maps/graph.json"] == "" {
		t.Fatal("partial progress unavailable", c)
	}
	objects.fail = false
	done := runGeneration(t, f, token, p.ID)
	if done.Unchanged {
		t.Fatal("partial job incorrectly skipped regeneration")
	}
	_ = f.db.View(func(st model.State) error {
		p := st.Projects[p.ID]
		if p.WikiIncomplete || len(st.Inbox) != 0 || len(st.Revisions) != 1 {
			t.Error("retry did not complete current wiki", p)
		}
		return nil
	})
	raw, err := objects.Get(context.Background(), blob.Key("projects/"+p.ID+"/.kioku/state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var content map[string]json.RawMessage
	json.Unmarshal(raw, &content)
	if _, ok := content["members"]; ok {
		t.Error("account membership stored in project content")
	}
}
