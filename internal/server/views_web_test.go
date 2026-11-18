package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func TestWikiViewPage(t *testing.T) {
	t.Parallel()
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
					for _, want := range []string{"View の設定", `method="post" action="` + api + `"`, `name="title" value="Notes"`, `name="prompt"`, "Organize decisions by date."} {
						if !strings.Contains(html, want) {
							t.Fatalf("owner page missing %q", want)
						}
					}
				} else if strings.Contains(html, "View の設定") || strings.Contains(html, "Organize decisions by date.") {
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
	page := browserPage(t, f, path, owner).Body.String()
	version := regexp.MustCompile(`name="version" value="([^"]+)"`).FindStringSubmatch(page)
	if len(version) != 2 {
		t.Fatal("form is missing the current settings version")
	}
	const savedPrompt = "First line\n</textarea><p>Escaped prompt</p>"
	form := url.Values{"title": {"Updated title"}, "prompt": {strings.ReplaceAll(savedPrompt, "\n", "\r\n")}, "version": {version[1]}}
	post := func(token, origin string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, api, strings.NewReader(form.Encode()))
		r.AddCookie(&http.Cookie{Name: "kioku_session", Value: token})
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		token, origin string
		status        int
	}{
		{owner, "", http.StatusForbidden},
		{owner, "https://elsewhere.example", http.StatusForbidden},
		{reader, f.s.Config.BaseURL, http.StatusForbidden},
		{owner, f.s.Config.BaseURL, http.StatusSeeOther},
	} {
		w := post(tc.token, tc.origin)
		if w.Code != tc.status || w.Code == http.StatusSeeOther && w.Header().Get("Location") != path {
			t.Fatalf("form status %d, want %d: %s", w.Code, tc.status, w.Body.String())
		}
	}
	page = browserPage(t, f, path, owner).Body.String()
	if !strings.Contains(page, "Updated title") || !strings.Contains(page, "&lt;/textarea&gt;") || strings.Contains(page, savedPrompt) {
		t.Fatal("saved settings were not rendered as escaped text", page)
	}
	form.Set("prompt", "stale edit")
	if w := post(owner, f.s.Config.BaseURL); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "stale edit") || strings.Contains(w.Body.String(), "<button>保存</button>") {
		t.Fatalf("stale edit was not retained and blocked: %d %s", w.Code, w.Body.String())
	}
	form.Set("prompt", " ")
	form.Set("title", "draft title")
	if w := post(owner, f.s.Config.BaseURL); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), `role="alert"`) || !strings.Contains(w.Body.String(), `value="draft title"`) {
		t.Fatalf("invalid edit was not retained: %d %s", w.Code, w.Body.String())
	}
	if code := f.request("GET", api, owner, nil, &detail); code != 200 || detail.Title != "Updated title" || detail.Prompt != savedPrompt {
		t.Fatalf("failed submission changed settings: %d %+v", code, detail)
	}
	if code := f.request("PATCH", api, owner, map[string]string{"title": "API\nTitle"}, &detail); code != http.StatusOK {
		t.Fatal("JSON title edit:", code)
	}
	form.Set("title", "APITitle")
	form.Set("prompt", "Changed in Web")
	form.Set("version", viewSettingsVersion(detail.viewDefinition))
	if w := post(owner, f.s.Config.BaseURL); w.Code != http.StatusSeeOther {
		t.Fatalf("form prompt edit returned %d: %s", w.Code, w.Body.String())
	}
	if code := f.request("GET", api, owner, nil, &detail); code != http.StatusOK || detail.Title != "API\nTitle" || detail.Prompt != "Changed in Web" {
		t.Fatalf("editing the prompt changed an unedited title: %d %+v", code, detail)
	}
	for _, token := range []string{"", f.login("wiki-outsider@example.com")} {
		if w := browserPage(t, f, path, token); w.Code != 404 || strings.Contains(w.Body.String(), "Changed in Web") {
			t.Fatalf("unauthorized page: %d", w.Code)
		}
	}
}

func TestViewFormAcceptsJSONSettingsWithMultibyteText(t *testing.T) {
	f, owner, p := orgProject(t)
	api := "/api/projects/" + p.Namespace + "/views/project-map"
	prompt := strings.Repeat("議", model.MaxJSONRequestBytes/9+1) + "\r\nLast line"
	var view viewDefinition
	if code := f.request("PATCH", api, owner, map[string]string{"prompt": prompt}, &view); code != http.StatusOK {
		t.Fatal("JSON settings:", code)
	}
	fields := url.Values{"title": {"New title"}, "prompt": {prompt}, "version": {viewSettingsVersion(view)}}.Encode()
	if len(fields) <= model.MaxJSONRequestBytes {
		t.Fatal("form fixture does not exercise percent encoding expansion")
	}
	r := httptest.NewRequest(http.MethodPost, api, strings.NewReader(fields))
	r.AddCookie(&http.Cookie{Name: "kioku_session", Value: owner})
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", f.s.Config.BaseURL)
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("form save returned %d", w.Code)
	}
	if code := f.request("GET", api, owner, nil, &view); code != http.StatusOK || view.Title != "New title" || view.Prompt != prompt {
		t.Fatalf("large settings were not saved: code %d, title %q, prompt bytes %d", code, view.Title, len(view.Prompt))
	}
}
