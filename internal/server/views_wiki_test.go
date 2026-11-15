package server

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/prompts"
)

func TestWikiViewDetail(t *testing.T) {
	f, owner, p := orgProject(t)
	path := "/api/projects/" + p.ID + "/views/wiki"
	updated := time.Date(2026, 11, 15, 7, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                  string
		published, incomplete bool
	}{
		{"unpublished", false, false}, {"published", true, false}, {"incomplete", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				project := fixtureProjectsGet(t, st, p.ID)
				project.WikiUpdatedAt = time.Time{}
				if tc.published {
					project.WikiUpdatedAt = updated
				}
				project.WikiIncomplete = tc.incomplete
				fixtureProjectsPut(t, st, p.ID, project)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if code := f.request("GET", path, owner, nil, &got); code != 200 {
				t.Fatal(code, got)
			}
			want := map[string]any{"view_id": "vw_" + auth.Hash(p.ID + "/wiki")[:20], "slug": "wiki", "title": "Wiki", "prompt": prompts.DefaultWikiPrompt(), "created_at": p.Created.Format(time.RFC3339Nano), "status": "pending", "wiki_updated_since_run": nil}
			if tc.published && !tc.incomplete {
				want["status"] = "completed"
				want["generated_at"] = updated.Format(time.RFC3339Nano)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("detail = %#v, want %#v", got, want)
			}
		})
	}
}

func TestWikiViewSettingsAndReservedSlug(t *testing.T) {
	f, owner, p := orgProject(t)
	path := "/api/projects/" + p.ID + "/views"
	id := newWikiViewRow(p.ID).ID
	patch := func(body any) viewDetail {
		t.Helper()
		var got viewDetail
		if code := f.request("PATCH", path+"/wiki", owner, body, &got); code != 200 {
			t.Fatalf("patch = %d: %+v", code, got)
		}
		return got
	}
	checkError := func(method, suffix, token string, body any, code int, message string) {
		t.Helper()
		var got map[string]any
		if status := f.request(method, path+suffix, token, body, &got); status != code || got["error"] != message {
			t.Fatalf("%s: %d %+v", method, status, got)
		}
	}
	checkError("POST", "", owner, map[string]string{"slug": "wiki", "title": "W", "prompt": "x"}, 409, "view slug is already in use")
	checkError("DELETE", "/wiki", owner, nil, 409, "the wiki view cannot be deleted")
	for _, body := range []map[string]any{{"title": " "}, {"prompt": nil}, {"prompt": 42}, {"title": false}} {
		checkError("PATCH", "/wiki", owner, body, 400, "view settings must contain text")
	}
	checkError("PATCH", "/wiki", owner, map[string]any{}, 400, "provide title or prompt with text")
	if got := patch(map[string]string{"title": "Notes"}); got.Title != "Notes" || got.Prompt != prompts.DefaultWikiPrompt() {
		t.Fatalf("title-only = %+v", got)
	}
	row := fixtureViewRow(t, f, p.ID, id)
	if row.CreatedAt != nil || row.BundleHash != nil || row.ViewEpoch != nil || valueString(row.Data.Title) != "Notes" || row.Data.Prompt != nil {
		t.Fatal(row)
	}
	if got := patch(map[string]string{"prompt": "Saved"}); got.Title != "Notes" || got.Prompt != "Saved" {
		t.Fatal(got)
	}
	if got := patch(map[string]string{"title": "New notes"}); got.Prompt != "Saved" {
		t.Fatal(got)
	}
	if got := patch(map[string]string{"title": "Both", "prompt": "Both prompt"}); got.Title != "Both" || got.Prompt != "Both prompt" {
		t.Fatal(got)
	}
	before := fixtureViewRow(t, f, p.ID, id)
	checkError("DELETE", "/wiki", owner, nil, 409, "the wiki view cannot be deleted")
	if after := fixtureViewRow(t, f, p.ID, id); !reflect.DeepEqual(before, after) {
		t.Fatal("delete changed settings", after)
	}
	var created viewDefinition
	if code := f.request("POST", path, owner, map[string]string{"title": "Wiki", "prompt": "x"}, &created); code != 201 || !strings.HasPrefix(created.Slug, "view-") || created.CreatedAt == nil {
		t.Fatalf("auto slug: %d %+v", code, created)
	}
}

func TestWikiViewAuthorization(t *testing.T) {
	f, owner, p := orgProject(t)
	reader := f.login("wiki-reader@example.com")
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		project := fixtureProjectsGet(t, st, p.ID)
		project.Members[fixtureSessionsGet(t, st, auth.Hash(reader)).UserID] = "member"
		fixtureProjectsPut(t, st, p.ID, project)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, read := testAgent(t, f, owner, p.ID, "read")
	_, write := testAgent(t, f, owner, p.ID, "write")
	path := "/api/projects/" + p.ID + "/views/wiki"
	for _, token := range []string{reader, read, write} {
		if code := f.request("GET", path, token, nil, nil); code != 200 {
			t.Fatal(code)
		}
		if code := f.request("PATCH", path, token, map[string]string{"prompt": "x"}, nil); code != 403 {
			t.Fatal(code)
		}
		if code := f.request("DELETE", path, token, nil, nil); code != 403 {
			t.Fatal(code)
		}
	}
	var got map[string]any
	if code := f.request("PATCH", path, reader, map[string]string{"prompt": "x"}, &got); code != 403 || got["error"] != "owner permission required" {
		t.Fatal(code, got)
	}
	if code := f.request("GET", path, f.login("wiki-outsider@example.com"), nil, nil); code != 404 {
		t.Fatal(code)
	}
}

// E8: Wiki settings are optional SQL rows; obsolete object records are never read.
func TestWikiViewDoesNotReadObsoleteRecords(t *testing.T) {
	f, owner, p := orgProject(t)
	api := "/api/projects/" + p.ID + "/views/wiki"
	fixtureCheck(t, f.stateStore.Read(t.Context(), func(q *store.Queries) error {
		_, err := q.ViewBySlug(p.ID, "wiki")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("unexpected settings row", err)
		}
		return nil
	}))
	for _, key := range []blob.Key{viewsKey(p.ID, "index.json"), viewKey(p.ID, newWikiViewRow(p.ID).ID, "view.json")} {
		fixtureCheck(t, f.s.Blobs.Put(t.Context(), key, []byte("broken")))
	}
	var got viewDetail
	if c := f.request("GET", api, owner, nil, &got); c != 200 || got.Prompt != prompts.DefaultWikiPrompt() {
		t.Fatal(c, got)
	}
	if c := f.request("PATCH", api, owner, map[string]string{"prompt": "Keep me"}, nil); c != 200 {
		t.Fatal(c)
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/views", owner, map[string]string{"slug": "temporary", "title": "Temp", "prompt": "x"}, nil); c != 201 {
		t.Fatal(c)
	}
	if c := f.request("DELETE", "/api/projects/"+p.ID+"/views/temporary", owner, nil, nil); c != 204 {
		t.Fatal(c)
	}
	_, err := f.s.storage.collectGarbage(t.Context(), p.ID)
	fixtureCheck(t, err)
	if c := f.request("GET", api, owner, nil, &got); c != 200 || got.Prompt != "Keep me" {
		t.Fatal(c, got)
	}
}

func TestWikiViewListCreationOrder(t *testing.T) {
	f, owner, p := orgProject(t)
	ctx := t.Context()
	path := "/api/projects/" + p.ID + "/views"
	var created viewDefinition
	if code := f.request("POST", path, owner, map[string]string{"slug": "a-first-alphabetically", "title": "A", "prompt": "x"}, &created); code != 201 || created.CreatedAt == nil {
		t.Fatal(code, created)
	}
	fixtureViewCreated(t, f, p.ID, created.ID, p.Created.Add(time.Second))
	unlock, err := f.s.lockProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	fixtureCheck(t, f.stateStore.Write(ctx, func(q *store.Queries) error {
		for _, slug := range []string{"b-legacy", "a-legacy"} {
			if err := q.PutView(store.ViewRow{ID: "vw_" + slug, ProjectID: p.ID, Slug: slug, Data: store.ViewData{Title: &slug, Prompt: nullableString("x"), Status: nullableString("pending")}}); err != nil {
				return err
			}
		}
		return nil
	}))
	views, err := f.s.listViewsLocked(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, v := range views {
		slugs = append(slugs, v.Slug)
	}
	want := []string{"wiki", "project-map", created.Slug, "a-legacy", "b-legacy"}
	if newWikiViewRow(p.ID).ID > "vw_"+auth.Hash(p.ID + "/project-map")[:20] {
		want[0], want[1] = want[1], want[0]
	}
	if !reflect.DeepEqual(slugs, want) {
		t.Fatal(slugs, want)
	}
	if views[1].CreatedAt == nil {
		t.Fatal("seed has no created_at")
	}
	stamp := time.Now().UTC()
	tied := []viewDefinition{{ID: "b", CreatedAt: &stamp}, {ID: "a", CreatedAt: &stamp}}
	sortViews(tied)
	if tied[0].ID != "a" {
		t.Fatal(tied)
	}
}

func TestWikiViewUsesConfiguredDefaultUntilPromptSaved(t *testing.T) {
	f, owner, p := orgProject(t)
	const override = "Organize decisions by date.\n"
	rendered, err := loadPrompts(t, prompts.Variables{}, map[string]string{prompts.DefaultPromptFile: override})
	if err != nil {
		t.Fatal(err)
	}
	f.s.Config.Prompts = rendered
	path := "/api/projects/" + p.ID + "/views/wiki"
	for _, tc := range []struct {
		method string
		body   any
		want   string
	}{
		{"GET", nil, override},
		{"PATCH", map[string]string{"title": "Notes"}, override},
		{"PATCH", map[string]string{"prompt": "Owner instructions"}, "Owner instructions"},
		{"GET", nil, "Owner instructions"},
	} {
		var got viewDetail
		if code := f.request(tc.method, path, owner, tc.body, &got); code != 200 || got.Prompt != tc.want {
			t.Fatal(code, got)
		}
	}
}
