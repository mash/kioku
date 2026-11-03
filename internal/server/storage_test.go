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
	"github.com/mash/kioku/internal/model"
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
	ts := httptest.NewServer(s.Handler())
	s.Config.BaseURL = ts.URL
	t.Cleanup(func() { ts.Close(); db.Close() })
	return &fixture{t: t, s: s, url: ts.URL, client: ts.Client(), db: db}
}
func runGeneration(t *testing.T, f *fixture, token, pid string) model.Job {
	t.Helper()
	var j model.Job
	base := "/api/projects/" + pid
	if c := f.request("POST", base+"/generate", token, nil, &j); c != 202 {
		t.Fatal(c)
	}
	if _, err := f.s.process(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	f.request("GET", base+"/jobs/"+j.ID, token, nil, &j)
	return j
}

func TestRestoringSQLiteDoesNotRollBackProjectContent(t *testing.T) {
	root, remote := t.TempDir(), t.TempDir()
	objects := blob.Local{Dir: remote}
	f := objectFixture(t, root, objects)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	base := "/api/projects/" + p.ID
	first := putFile(t, f, token, p.ID, "first.md", "First source")
	var inbox model.Snapshot
	if c := f.request("GET", base+"/snapshot", token, nil, &inbox); c != 200 || !strings.Contains(inbox.Files["wiki/inbox.md"], "first.md") {
		t.Fatal("put not visible before generation", c, inbox)
	}
	runGeneration(t, f, token, p.ID)
	backup := filepath.Join(t.TempDir(), "backup")
	if err := f.db.Backup(backup); err != nil {
		t.Fatal(err)
	}
	putFile(t, f, token, p.ID, "second.md", "Newer than SQLite backup")
	runGeneration(t, f, token, p.ID)
	journal, err := objects.List(context.Background(), "projects/"+p.ID+"/wiki/inbox/")
	if err != nil || len(journal) != 2 {
		t.Fatal("intake journal missing", journal, err)
	}
	originalEntry, err := objects.Get(context.Background(), journal[0])
	if err != nil {
		t.Fatal(err)
	}
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
		if len(st.Sources) != 2 || len(st.Revisions) != 1 || st.Projects[p.ID].SourceVersion != 2 || st.Projects[p.ID].ProcessedIntakeSequence != 2 {
			t.Error("content/cursor did not survive config restore")
		}
		if st.Sources[first.SourceIDs[0]].Title != "first.md" {
			t.Error("source metadata lost")
		}
		return nil
	})
	keys, err := objects.List(context.Background(), "projects/"+p.ID+"/")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if strings.Contains(key, "/revisions/") {
			t.Error("full wiki history retained", key)
		}
	}
	after, _ := objects.Get(context.Background(), journal[0])
	if string(after) != string(originalEntry) {
		t.Error("generation rewrote append-only intake")
	}
	if _, err = os.Stat(filepath.Join(backup, "projects")); !os.IsNotExist(err) {
		t.Fatal("content databases included in config backup")
	}
}

type failCurrentWiki struct {
	blob.Store
	fail bool
}

func (b *failCurrentWiki) Put(ctx context.Context, key string, body []byte) error {
	if b.fail && strings.HasSuffix(key, "/wiki/index.md") {
		return errors.New("temporary R2 upload failure")
	}
	return b.Store.Put(ctx, key, body)
}
func TestInterruptedMutableWikiCanBeRetried(t *testing.T) {
	objects := &failCurrentWiki{Store: blob.Local{Dir: t.TempDir()}, fail: true}
	f := objectFixture(t, t.TempDir(), objects)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	putFile(t, f, token, p.ID, "note.md", "Durable input")
	var job model.Job
	base := "/api/projects/" + p.ID
	f.request("POST", base+"/generate", token, nil, &job)
	if _, err := f.s.process(context.Background(), job); err == nil {
		t.Fatal("failed upload reported success")
	}
	_ = f.db.View(func(st model.State) error {
		p := st.Projects[p.ID]
		if !p.WikiIncomplete || p.WikiSourceVersion == p.SourceVersion {
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
		if p.WikiIncomplete || p.WikiSourceVersion != p.SourceVersion || len(st.Revisions) != 1 {
			t.Error("retry did not complete current wiki", p)
		}
		return nil
	})
	raw, err := objects.Get(context.Background(), "projects/"+p.ID+"/.kioku/state.json")
	if err != nil {
		t.Fatal(err)
	}
	var content map[string]json.RawMessage
	json.Unmarshal(raw, &content)
	if _, ok := content["members"]; ok {
		t.Error("account membership stored in project content")
	}
}
