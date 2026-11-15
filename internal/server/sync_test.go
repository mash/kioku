package server

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

func (f *fixture) readWiki(base, token string) map[string]string {
	f.t.Helper()
	var manifest model.SyncManifest
	if code := f.request("GET", base+"/snapshot?manifest=1", token, nil, &manifest); code != 200 {
		f.t.Fatalf("wiki manifest: HTTP %d", code)
	}
	pages := make(map[string]string, len(manifest.Files))
	for name := range manifest.Files {
		var file model.SnapshotFile
		if code := f.request("GET", base+"/snapshot?file="+url.QueryEscape(name), token, nil, &file); code != 200 {
			f.t.Fatalf("wiki file %q: HTTP %d", name, code)
		}
		pages[name] = file.Body
	}
	return pages
}

func TestSyncManifestAndFileRespectMembershipAndPartialPublication(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	other := f.login("other@example.com")
	var p model.Project
	if code := f.request("POST", "/api/projects", token, map[string]string{"name": "sync"}, &p); code != 201 {
		t.Fatal(code)
	}
	endpoint := "/api/projects/" + p.ID + "/snapshot"
	var manifest model.SyncManifest
	if code := f.request("GET", endpoint+"?manifest=1", token, nil, &manifest); code != 200 || manifest.Files == nil || len(manifest.Files) != 0 {
		t.Fatal("empty wiki manifest", code, manifest)
	}
	key := blob.Key("projects/" + p.ID + "/wiki/index.md")
	if err := f.s.Blobs.Put(context.Background(), key, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := f.stateStore.Update(func(st *model.State) error {
		current := st.Projects[p.ID]
		current.Revision = "revision"
		current.WikiIncomplete = true
		st.Projects[p.ID] = current
		st.Revisions["revision"] = model.Revision{
			ID: "revision", ProjectID: p.ID,
			Files:      map[string]blob.Key{"wiki/index.md": key, "maps/graph.json": "missing-object"},
			FileHashes: map[string]string{"wiki/index.md": auth.Hash("first")},
			FileBytes:  map[string]int64{"wiki/index.md": 5},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code := f.request("GET", endpoint+"?manifest=1", token, nil, &manifest); code != 200 || len(manifest.Files) != 1 || manifest.Files["wiki/index.md"].Hash != auth.Hash("first") {
		t.Fatal(code, manifest)
	}
	for _, query := range []string{"?manifest=1", "?file=wiki%2Findex.md"} {
		if code := f.request("GET", endpoint+query, other, nil, nil); code != http.StatusNotFound {
			t.Fatal("membership bypass", code)
		}
		if code := f.request("GET", endpoint+query, "", nil, nil); code != http.StatusUnauthorized {
			t.Fatal(code)
		}
	}
	// The file endpoint returns the current bytes even if publication advanced
	// between the manifest and file requests. The client verifies their own hash.
	if err := f.s.Blobs.Put(context.Background(), key, []byte("second")); err != nil {
		t.Fatal(err)
	}
	var file model.SnapshotFile
	if code := f.request("GET", endpoint+"?file=wiki%2Findex.md", token, nil, &file); code != 200 || file.Body != "second" || file.Hash != auth.Hash(file.Body) {
		t.Fatal(code, file)
	}
	for _, query := range []string{"", "?revision=" + manifest.Revision, "?file=maps%2Fgraph.json", "?file=..%2Fsecret"} {
		if code := f.request("GET", endpoint+query, token, nil, nil); code != http.StatusBadRequest {
			t.Fatal(query, code)
		}
	}
	if code := f.request("GET", endpoint+"?file=wiki%2Fmissing.md", token, nil, nil); code != 404 {
		t.Fatal(code)
	}
}
