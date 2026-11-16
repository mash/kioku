package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

func TestWikiViewPage(t *testing.T) {
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
	path := "/" + p.Namespace + "/views/wiki"
	api := "/api/projects/" + p.Namespace + "/views/wiki"
	if w := browserPage(t, f, path, owner); w.Code != 200 || !strings.Contains(w.Body.String(), "<h1>Wiki</h1>") {
		t.Fatalf("default page: %d %s", w.Code, w.Body.String())
	}
	if code := f.request("PATCH", api, owner, map[string]string{"title": "Notes", "prompt": "Organize decisions by date."}, nil); code != 200 {
		t.Fatal(code)
	}
	var detail viewDetail
	if code := f.request("GET", api, owner, nil, &detail); code != 200 || detail.Title != "Notes" || detail.Prompt != "Organize decisions by date." {
		t.Fatalf("saved settings: %d %+v", code, detail)
	}
	updated := time.Date(2026, 11, 15, 7, 30, 0, 0, time.UTC)
	for _, state := range []string{"unpublished", "published"} {
		t.Run(state, func(t *testing.T) {
			if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				project := fixtureProjectsGet(t, st, p.ID)
				project.WikiUpdatedAt = updated
				if state == "unpublished" {
					project.WikiUpdatedAt = time.Time{}
				}
				fixtureProjectsPut(t, st, p.ID, project)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			status := "未生成"
			if state == "published" {
				status = "生成済み"
			}
			for _, token := range []string{owner, reader} {
				w := browserPage(t, f, path, token)
				html := w.Body.String()
				for _, want := range []string{"<h1>Notes</h1>", "<p>" + status + "</p>", `href="/` + p.Namespace + `">Wiki</a>`} {
					if w.Code != 200 || !strings.Contains(html, want) {
						t.Fatalf("page missing %q: %d %s", want, w.Code, html)
					}
				}
				if strings.Contains(html, "<iframe") || strings.Contains(html, "View を削除") || strings.Contains(html, `data-view-action="DELETE"`) {
					t.Fatal("wiki page contains bundle or deletion form")
				}
				if strings.Contains(html, "2026-11-15 07:30 UTC") != (state == "published") {
					t.Fatal("incorrect update time", html)
				}
				policy := pageCSP + "; img-src data:"
				if token == owner {
					policy += viewSettingsCSP
					for _, want := range []string{"View の設定", `action="` + api + `" data-view-action="PATCH"`, `<fieldset disabled>`, `name="title"`, `name="prompt"`, viewSettingsScriptTag} {
						if !strings.Contains(html, want) {
							t.Fatalf("owner page missing %q", want)
						}
					}
				} else if strings.Contains(html, "View の設定") || strings.Contains(html, `data-view-action="PATCH"`) {
					t.Fatal("member can see settings")
				}
				if w.Header().Get("Content-Security-Policy") != policy {
					t.Fatal("unexpected CSP", w.Header())
				}
				w = browserPage(t, f, "/"+p.Namespace, token)
				html = w.Body.String()
				first := strings.Index(html, `href="`+path+`">Notes</a>`)
				second := strings.Index(html, `href="/`+p.Namespace+`/views/project-map"`)
				if w.Code != 200 || first < 0 || second < 0 {
					t.Fatalf("view list entry missing: %d %s", w.Code, html)
				}
				end := strings.Index(html[first:], "</li>")
				if end < 0 {
					t.Fatal("row missing")
				}
				item := html[first : first+end]
				if !strings.Contains(item, status) || strings.Contains(item, "2026-11-15 07:30 UTC") != (state == "published") {
					t.Fatal("incorrect list metadata", item)
				}
			}
		})
	}
	for _, token := range []string{"", f.login("wiki-outsider@example.com")} {
		if w := browserPage(t, f, path, token); w.Code != 404 || strings.Contains(w.Body.String(), "<h1>Notes</h1>") {
			t.Fatalf("unauthorized page: %d", w.Code)
		}
	}
}
