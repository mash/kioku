package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func syncFixture(t *testing.T) (*App, *map[string]string, *map[string]int, *int) {
	t.Helper()
	files := map[string]string{"wiki/index.md": "# initial", "wiki/old.md": "old"}
	hits := map[string]int{}
	status := 200
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/projects") {
			json.NewEncoder(w).Encode([]model.Project{{ID: "project", Namespace: "org/project"}})
			return
		}
		name := r.URL.Query().Get("file")
		hits[name]++
		if status != 200 && name == "" {
			w.WriteHeader(status)
			return
		}
		if name != "" {
			body, ok := files[name]
			if !ok {
				w.WriteHeader(404)
				return
			}
			if body == "fail" {
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(model.SnapshotFile{Body: body, Hash: auth.Hash(body)})
			return
		}
		manifest := model.SyncManifest{Revision: "same-publication", Files: map[string]model.SyncFile{}}
		for n, b := range files {
			manifest.Files[n] = model.SyncFile{Hash: auth.Hash(b), Bytes: int64(len(b))}
		}
		json.NewEncoder(w).Encode(manifest)
	}))
	t.Cleanup(ts.Close)
	a := &App{Dir: t.TempDir(), Config: Config{Server: ts.URL, Project: "project", Namespace: "org/project"}, Client: ts.Client(), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, ctx: context.Background()}
	return a, &files, &hits, &status
}
func TestSyncDeltaOverwriteDeleteAndPartialFailure(t *testing.T) {
	a, files, hits, _ := syncFixture(t)
	r, err := a.syncWiki()
	if err != nil || len(r.Added) != 2 {
		t.Fatal(r, err)
	}
	inbox := filepath.Join(a.localDir(), "inbox", "draft.md")
	os.WriteFile(inbox, []byte("private draft"), 0600)
	_, err = a.syncWiki()
	if err != nil || (*hits)["wiki/index.md"] != 1 {
		t.Fatal("unchanged body fetched", *hits, err)
	}
	path := filepath.Join(a.localDir(), "wiki", "index.md")
	os.Chmod(path, 0600)
	os.WriteFile(path, []byte("local edit"), 0600)
	r, err = a.syncWiki()
	if err != nil || len(r.Updated) != 1 {
		t.Fatal("edit not replaced", r, err)
	}
	delete(*files, "wiki/old.md")
	(*files)["wiki/index.md"] = "# changed"
	(*files)["wiki/broken.md"] = "fail"
	r, err = a.syncWiki()
	if err == nil || len(r.Deleted) != 1 || len(r.Updated) != 1 {
		t.Fatal("partial result lost", r, err)
	}
	var partial manifest
	checkpoint, _ := os.ReadFile(filepath.Join(a.metadataDir(), "manifest.json"))
	json.Unmarshal(checkpoint, &partial)
	if !partial.Incomplete {
		t.Fatal("partial sync reported complete")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "# changed" {
		t.Fatal(string(b))
	}
	if _, err = os.Stat(filepath.Join(a.localDir(), "wiki", "old.md")); !os.IsNotExist(err) {
		t.Fatal("obsolete file retained", err)
	}
	b, _ = os.ReadFile(inbox)
	if string(b) != "private draft" {
		t.Fatal("inbox changed")
	}
	(*files)["wiki/broken.md"] = "fixed"
	r, err = a.syncWiki()
	if err != nil || len(r.Added) != 1 || len(r.Updated) != 0 {
		t.Fatal(r, err)
	}
}
func TestSyncRejectsSymlinkAndRetainsWikiOnRevocation(t *testing.T) {
	a, _, _, status := syncFixture(t)
	if _, err := a.syncWiki(); err != nil {
		t.Fatal(err)
	}
	*status = 403
	if _, err := a.syncWiki(); !errors.Is(err, errSyncForbidden) {
		t.Fatal(err)
	}
	path := filepath.Join(a.localDir(), "wiki", "index.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	*status = 200
	os.Remove(path)
	victim := filepath.Join(t.TempDir(), "private")
	os.WriteFile(victim, []byte("private"), 0600)
	os.Symlink(victim, path)
	if _, err := a.syncWiki(); err == nil {
		t.Fatal("symlink followed")
	}
	b, _ := os.ReadFile(victim)
	if string(b) != "private" {
		t.Fatal("outside file changed")
	}
}

func TestSyncRetryAndAuthenticationFailure(t *testing.T) {
	hits := 0
	status := 503
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(status) }))
	defer ts.Close()
	a := &App{Config: Config{Server: ts.URL}, Client: ts.Client(), ctx: context.Background()}
	if _, err := a.syncRequest("/test", nil); err == nil || hits != 3 {
		t.Fatal(err, hits)
	}
	status = 401
	hits = 0
	if _, err := a.syncRequest("/test", nil); err == nil || hits != 1 {
		t.Fatal(err, hits)
	}
}

func TestPullCommandArguments(t *testing.T) {
	a, _, hits, _ := syncFixture(t)
	if err := a.dispatch([]string{"pull"}); err != nil {
		t.Fatal(err)
	}
	clear(*hits)
	if err := a.dispatch([]string{"pull", "--typo"}); err == nil {
		t.Fatal("invalid pull arguments accepted")
	}
	if err := a.dispatch([]string{"sync", "--all"}); err == nil {
		t.Fatal("removed sync command accepted")
	}
	if len(*hits) != 0 {
		t.Fatal("invalid commands issued requests", *hits)
	}
}
