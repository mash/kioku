package server

import (
	"context"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

func TestViewSettingsAuthorizationAndShape(t *testing.T) {
	f, owner, project := orgProject(t)
	readMember := f.login("reader@example.com")
	outsider := f.login("outsider@example.com")
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		p := fixtureProjectsGet(t, st, project.ID)
		p.Members[fixtureSessionsGet(t, st, auth.Hash(readMember)).UserID] = "member"
		fixtureProjectsPut(t, st, p.ID, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, readToken := testAgent(t, f, owner, project.ID, "read")
	_, writeToken := testAgent(t, f, owner, project.ID, "write")

	path := "/api/projects/" + project.ID + "/views"
	var seed viewDetail
	if code := f.request("GET", path+"/project-map", readMember, nil, &seed); code != 200 || seed.Status != "pending" || seed.WikiUpdatedSinceRun != nil || seed.Prompt == "" {
		t.Fatalf("default view returned %d: %+v", code, seed)
	}
	payload := map[string]string{"slug": "delivery", "title": "Delivery", "prompt": "Show open work"}
	for _, tc := range []struct {
		token string
		code  int
	}{{"", 401}, {outsider, 404}, {readMember, 403}, {readToken, 403}, {writeToken, 403}} {
		if code := f.request("POST", path, tc.token, payload, nil); code != tc.code {
			t.Errorf("create view returned %d, want %d", code, tc.code)
		}
	}
	delivery := path + "/delivery"
	var created viewDefinition
	if code := f.request("POST", path, owner, payload, &created); code != 201 || created.Slug != "delivery" || created.Status != "pending" || created.ID == "" || created.Prompt != payload["prompt"] {
		t.Fatalf("create view returned %d: %+v", code, created)
	}
	if code := f.request("POST", path, owner, payload, nil); code != 409 {
		t.Errorf("duplicate view returned %d", code)
	}
	for _, token := range []string{readMember, readToken, writeToken} {
		var detail map[string]any
		if code := f.request("GET", delivery, token, nil, &detail); code != 200 {
			t.Fatalf("detail returned %d", code)
		}
		if stale, present := detail["wiki_updated_since_run"]; detail["prompt"] != payload["prompt"] || !present || stale != nil {
			t.Errorf("unexpected detail: %v", detail)
		}
		for _, key := range []string{"job_id", "generated_at", "wiki_snapshot_id", "settings_generation"} {
			if _, present := detail[key]; present {
				t.Errorf("unexpected field %s in ungenerated view", key)
			}
		}
	}
	if code := f.request("GET", delivery, outsider, nil, nil); code != 404 {
		t.Errorf("non-member read returned %d", code)
	}
	var otherProject fixtureProject
	if code := f.request("POST", "/api/projects", owner, map[string]string{"name": "acme/other-platform"}, &otherProject); code != 201 {
		t.Fatal(code)
	}
	if code := f.request("GET", "/api/projects/"+otherProject.ID+"/views/project-map", readToken, nil, nil); code != 404 {
		t.Errorf("cross-project token read returned %d", code)
	}

	for _, invalid := range []map[string]any{{}, {"prompt": " "}, {"title": nil, "prompt": "Valid prompt"}, {"prompt": 42}} {
		if code := f.request("PATCH", delivery, owner, invalid, nil); code != 400 {
			t.Errorf("invalid patch returned %d for %v", code, invalid)
		}
	}
	var updated viewDefinition
	if code := f.request("PATCH", delivery, owner, map[string]string{"prompt": "Updated"}, &updated); code != 200 || updated.ID != created.ID || updated.Prompt != "Updated" || updated.Title != created.Title {
		t.Fatalf("patch returned %d: %+v", code, updated)
	}
	if code := f.request("DELETE", delivery, owner, nil, nil); code != 204 {
		t.Fatal("delete", code)
	}
	if code := f.request("GET", delivery, owner, nil, nil); code != 404 {
		t.Fatal("deleted detail", code)
	}
}

func TestViewDetailComparesWithFullyPublishedWiki(t *testing.T) {
	f, owner, p := orgProject(t)
	ctx := t.Context()
	setCompleteViewWiki(t, f, p.ID, "wiki-1", "epoch-1")
	unlock, err := f.s.storage.lock(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer unlock()
		view, err := f.s.viewBySlugLocked(ctx, p.ID, "project-map")
		if err != nil {
			t.Fatal(err)
		}
		fixtureViewBundle(t, f, p.ID, view.ID, "view-job", "wiki-1", []byte("bundle"))
	}()
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		p := fixtureProjectsGet(t, st, p.ID)
		p.Revision, p.WikiIncomplete = "wiki-2", true
		fixtureProjectsPut(t, st, p.ID, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := "/api/projects/" + p.ID + "/views/project-map"
	var detail viewDetail
	if code := f.request("GET", path, owner, nil, &detail); code != 200 || detail.WikiUpdatedSinceRun == nil || *detail.WikiUpdatedSinceRun {
		t.Fatalf("incomplete Wiki was counted as an update, response %d: %+v", code, detail)
	}
	setCompleteViewWiki(t, f, p.ID, "wiki-2", "epoch-1")
	if code := f.request("GET", path, owner, nil, &detail); code != 200 || detail.WikiUpdatedSinceRun == nil || !*detail.WikiUpdatedSinceRun {
		t.Fatalf("completed Wiki was not counted as an update, response %d: %+v", code, detail)
	}
}
