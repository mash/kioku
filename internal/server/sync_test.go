package server

import (
	"context"
	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"net/http"
	"testing"
)

func TestSyncManifestAndFileRespectMembershipAndPartialPublication(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	other := f.login("other@example.com")
	var p model.Project
	if code := f.request("POST", "/api/projects", token, map[string]string{"name": "sync"}, &p); code != 201 {
		t.Fatal(code)
	}
	key := "projects/" + p.ID + "/wiki/index.md"
	if err := f.s.Blobs.Put(context.Background(), key, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Update(func(st *model.State) error {
		current := st.Projects[p.ID]
		current.Revision = "revision"
		current.WikiIncomplete = true
		st.Projects[p.ID] = current
		st.Revisions["revision"] = model.Revision{ID: "revision", ProjectID: p.ID, Files: map[string]string{"wiki/index.md": key}, FileHashes: map[string]string{"wiki/index.md": auth.Hash("first")}, FileBytes: map[string]int64{"wiki/index.md": 5}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/projects/" + p.ID + "/snapshot"
	var manifest model.SyncManifest
	if code := f.request("GET", endpoint+"?manifest=1", token, nil, &manifest); code != 200 || manifest.Files["wiki/index.md"].Hash != auth.Hash("first") {
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
	if code := f.request("GET", endpoint+"?file=..%2Fsecret", token, nil, nil); code != 400 {
		t.Fatal(code)
	}
	if code := f.request("GET", endpoint+"?file=wiki%2Fmissing.md", token, nil, nil); code != 404 {
		t.Fatal(code)
	}
}
