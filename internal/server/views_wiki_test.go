package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
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
			if err := f.stateStore.Update(func(st *model.State) error {
				project := st.Projects[p.ID]
				project.WikiUpdatedAt = time.Time{}
				if tc.published {
					project.WikiUpdatedAt = updated
				}
				project.WikiIncomplete = tc.incomplete
				st.Projects[p.ID] = project
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
	ctx := t.Context()
	id := newWikiViewRecord(p.ID).ID
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
	var disk map[string]any
	if err := f.s.readViewRecord(ctx, viewKey(p.ID, id, "view.json"), &disk); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(disk, map[string]any{"view_id": id, "slug": "wiki", "title": "Notes"}) {
		t.Fatalf("title-only record = %v", disk)
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
	disk = nil
	if err := f.s.readViewRecord(ctx, viewKey(p.ID, id, "view.json"), &disk); err != nil {
		t.Fatal(err)
	}
	if len(disk) != 4 {
		t.Fatalf("unexpected record fields: %v", disk)
	}
	var index struct {
		Views map[string]map[string]any `json:"views"`
	}
	if err := f.s.readViewRecord(ctx, viewsKey(p.ID, "index.json"), &index); err != nil {
		t.Fatal(err)
	}
	if entry := index.Views["wiki"]; len(entry) != 2 || entry["id"] != id || entry["settings_generation"] == "" {
		t.Fatalf("index = %v", index)
	}
	before, err := f.s.Blobs.Get(ctx, viewsKey(p.ID, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	checkError("DELETE", "/wiki", owner, nil, 409, "the wiki view cannot be deleted")
	after, err := f.s.Blobs.Get(ctx, viewsKey(p.ID, "index.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("delete changed index", err)
	}
	var created viewDefinition
	if code := f.request("POST", path, owner, map[string]string{"title": "Wiki", "prompt": "x"}, &created); code != 201 || !strings.HasPrefix(created.Slug, "view-") || created.CreatedAt == nil {
		t.Fatalf("auto slug: %d %+v", code, created)
	}
}

func TestWikiViewAuthorization(t *testing.T) {
	f, owner, p := orgProject(t)
	reader := f.login("wiki-reader@example.com")
	if err := f.stateStore.Update(func(st *model.State) error {
		project := st.Projects[p.ID]
		project.Members[st.Sessions[auth.Hash(reader)].UserID] = "member"
		st.Projects[p.ID] = project
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

func TestWikiViewCorruptionAndRepair(t *testing.T) {
	for _, body := range []string{"broken", `{"view_id":"wrong","slug":"wiki"}`, `{"view_id":"id","slug":"wrong"}`, "null", ""} {
		t.Run(body, func(t *testing.T) {
			f, owner, p := orgProject(t)
			ctx := t.Context()
			path := "/api/projects/" + p.ID + "/views/wiki"
			if code := f.request("PATCH", path, owner, map[string]string{"prompt": "Old"}, nil); code != 200 {
				t.Fatal(code)
			}
			key := viewKey(p.ID, newWikiViewRecord(p.ID).ID, "view.json")
			if body == "" {
				if err := f.s.Blobs.Delete(ctx, key); err != nil {
					t.Fatal(err)
				}
			} else if err := f.s.Blobs.Put(ctx, key, []byte(body)); err != nil {
				t.Fatal(err)
			}
			if code := f.request("GET", path, owner, nil, nil); code != 500 {
				t.Fatal(code)
			}
			unlock, err := f.s.lockProject(ctx, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.s.listViewsLocked(ctx, p.ID)
			unlock()
			if err == nil {
				t.Fatal("corrupt wiki listed")
			}
			r := httptest.NewRequest(http.MethodGet, "/"+p.Namespace, nil)
			r.AddCookie(&http.Cookie{Name: "kioku_session", Value: owner})
			w := httptest.NewRecorder()
			f.s.Handler().ServeHTTP(w, r)
			if w.Code != 500 {
				t.Fatalf("project page = %d", w.Code)
			}
			var got viewDetail
			if code := f.request("PATCH", path, owner, map[string]string{"title": "Repaired"}, &got); code != 200 || got.Prompt != prompts.DefaultWikiPrompt() {
				t.Fatal(code, got)
			}
			if code := f.request("GET", path, owner, nil, nil); code != 200 {
				t.Fatal(code)
			}
			if err := f.s.Blobs.Put(ctx, viewsKey(p.ID, "index.json"), []byte("broken")); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{"GET", "PATCH"} {
				if code := f.request(method, path, owner, map[string]string{"prompt": "x"}, nil); code != 500 {
					t.Fatalf("bad index %s = %d", method, code)
				}
			}
		})
	}
}

func TestWikiViewSurvivesIndexWritesAndCollection(t *testing.T) {
	f, owner, p := orgProject(t)
	ctx := t.Context()
	path := "/api/projects/" + p.ID + "/views"
	if code := f.request("PATCH", path+"/wiki", owner, map[string]string{"prompt": "Keep me"}, nil); code != 200 {
		t.Fatal(code)
	}
	if code := f.request("POST", path, owner, map[string]string{"slug": "temporary", "title": "Temp", "prompt": "x"}, nil); code != 201 {
		t.Fatal(code)
	}
	if code := f.request("DELETE", path+"/temporary", owner, nil, nil); code != 204 {
		t.Fatal(code)
	}
	setCompleteViewWiki(t, f, p.ID, "wiki-1", "epoch-1")
	func() {
		unlock, err := f.s.lockProject(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if err := f.s.seedViewsLocked(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		title := "Updated map"
		view, err := f.s.updateViewLocked(ctx, p.ID, "project-map", &title, nil)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := f.s.publishViewLocked(ctx, p.ID, view.ID, view.Version, "job", "wiki-1", []byte("bundle")); !ok || err != nil {
			t.Fatal(ok, err)
		}
		setCompleteViewWiki(t, f, p.ID, "wiki-2", "epoch-2")
		if err := f.s.invalidateViewsLocked(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.s.collectViewsLocked(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		index, err := f.s.readViewIndexLocked(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := index.Views["wiki"]; ok || index.Wiki == nil {
			t.Fatalf("index = %+v", index)
		}
		if _, err := f.s.viewBySlugLocked(ctx, p.ID, "wiki"); err == nil {
			t.Fatal("ordinary lookup found wiki")
		}
		body, err := json.Marshal(index)
		if err != nil {
			t.Fatal(err)
		}
		var roundtrip viewIndex
		if err := json.Unmarshal(body, &roundtrip); err != nil || !reflect.DeepEqual(roundtrip, index) {
			t.Fatal("index roundtrip", err)
		}
	}()
	var got viewDetail
	if code := f.request("GET", path+"/wiki", owner, nil, &got); code != 200 || got.Prompt != "Keep me" {
		t.Fatal(code, got)
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
	unlock, err := f.s.lockProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	index, err := f.s.readViewIndexLocked(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"b-legacy", "a-legacy"} {
		id := "vw_" + slug
		if err := f.s.writeViewRecord(ctx, viewKey(p.ID, id, "view.json"), viewDefinition{ID: id, Slug: slug, Title: slug, Prompt: "x", Status: "pending"}); err != nil {
			t.Fatal(err)
		}
		index.Views[slug] = viewIndexEntry{ID: id, Version: "v"}
	}
	if err := f.s.writeViewRecord(ctx, viewsKey(p.ID, "index.json"), index); err != nil {
		t.Fatal(err)
	}
	views, err := f.s.listViewsLocked(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, v := range views {
		slugs = append(slugs, v.Slug)
	}
	if want := []string{"wiki", "project-map", created.Slug, "a-legacy", "b-legacy"}; !reflect.DeepEqual(slugs, want) {
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
	dir := t.TempDir()
	const override = "Organize decisions by date.\n"
	if err := os.WriteFile(filepath.Join(dir, prompts.DefaultPromptFile), []byte(override), 0600); err != nil {
		t.Fatal(err)
	}
	rendered, err := prompts.Load(dir, prompts.Variables{}, nil)
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
