package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func TestWikiViewPage(t *testing.T) {
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
	get := func(path, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			r.AddCookie(&http.Cookie{Name: "kioku_session", Value: token})
		}
		w := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(w, r)
		return w
	}
	path := "/" + p.Namespace + "/views/wiki"
	api := "/api/projects/" + p.Namespace + "/views/wiki"
	if w := get(path, owner); w.Code != 200 || !strings.Contains(w.Body.String(), "<h1>Wiki</h1>") {
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
	for _, state := range []string{"unpublished", "published", "incomplete"} {
		t.Run(state, func(t *testing.T) {
			if err := f.stateStore.Update(func(st *model.State) error {
				project := st.Projects[p.ID]
				project.WikiUpdatedAt = updated
				if state == "unpublished" {
					project.WikiUpdatedAt = time.Time{}
				}
				project.WikiIncomplete = state == "incomplete"
				st.Projects[p.ID] = project
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			status := "未生成"
			if state == "published" {
				status = "生成済み"
			}
			for _, token := range []string{owner, reader} {
				w := get(path, token)
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
				w = get("/"+p.Namespace, token)
				html = w.Body.String()
				first := strings.Index(html, `href="`+path+`">Notes</a>`)
				second := strings.Index(html, `href="/`+p.Namespace+`/views/project-map"`)
				if w.Code != 200 || first < 0 || second <= first {
					t.Fatalf("wiki is not first: %d %s", w.Code, html)
				}
				item := html[first:second]
				if !strings.Contains(item, status) || strings.Contains(item, "2026-11-15 07:30 UTC") != (state == "published") {
					t.Fatal("incorrect list metadata", item)
				}
			}
		})
	}
	for _, token := range []string{"", f.login("wiki-outsider@example.com")} {
		if w := get(path, token); w.Code != 404 || strings.Contains(w.Body.String(), "<h1>Notes</h1>") {
			t.Fatalf("unauthorized page: %d", w.Code)
		}
	}
}
