package server

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

type viewDetail struct {
	viewDefinition
	WikiUpdatedSinceRun *bool `json:"wiki_updated_since_run"`
}

func viewHasNewWiki(view viewDefinition, project model.Project) *bool {
	// The Wiki View is the Wiki itself, so it has no Wiki to compare with.
	if view.GeneratedAt == nil || view.Slug == wikiViewSlug {
		return nil
	}
	updated := view.WikiSnapshotID != project.Revision
	return &updated
}

type viewCreateRequest struct {
	Slug   json.RawMessage `json:"slug"`
	Title  string          `json:"title"`
	Prompt string          `json:"prompt"`
}

type viewListItem struct {
	ID          string     `json:"view_id"`
	Slug        string     `json:"slug"`
	Title       string     `json:"title"`
	GeneratedAt *time.Time `json:"generated_at"`
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
}

func viewListItems(views []viewDefinition) []viewListItem {
	items := make([]viewListItem, 0, len(views))
	for _, view := range views {
		items = append(items, viewListItem{view.ID, view.Slug, view.Title, view.GeneratedAt, view.Status, view.Error})
	}
	return items
}

func (s *Server) listViews(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	unlock, err := s.storage.lock(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		_, err := member(q, pid, uid)
		return err
	}); err != nil {
		return err
	}
	views, err := s.listViewsLocked(r.Context(), pid)
	if err != nil {
		return err
	}
	jsonOut(w, 200, map[string]any{"views": viewListItems(views)})
	return nil
}

func (s *Server) createView(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	unlock, err := s.storage.lock(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	view, err := s.createViewLocked(r.Context(), pid, readBody(w, r))
	if err != nil {
		return err
	}
	jsonOut(w, 201, view)
	return nil
}

func (s *Server) getView(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	unlock, err := s.storage.lock(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	var project model.Project
	err = s.Store.Read(r.Context(), func(q *store.Queries) error {
		var err error
		project, err = member(q, pid, uid)
		return err
	})
	if err != nil {
		return err
	}
	view, err := s.viewBySlugLocked(r.Context(), project.ID, r.PathValue("view"))
	if err != nil {
		return err
	}
	jsonOut(w, 200, viewDetail{view, viewHasNewWiki(view, project)})
	return nil
}

func (s *Server) updateView(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	form := r.Method == http.MethodPost
	var limit int64 = model.MaxJSONRequestBytes
	if form {
		// Check ownership before reading the larger browser request. The
		// mutation rechecks permissions before it parses or stores settings.
		if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
			_, err := owner(q, pid, uid)
			return err
		}); err != nil {
			return err
		}
		// Bound percent encoding separately from the decoded form, which
		// counts both bytes of CRLF line breaks. Reserve space for metadata.
		limit = 3*model.MaxJSONRequestBytes + 256
	}
	body := readBoundedBody(w, r, limit)
	unlock, err := s.storage.lock(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	// Decode inside the mutation so permission errors still precede validation.
	namespace := r.PathValue("org") + "/" + r.PathValue("slug")
	slug := r.PathValue("view")
	var edit viewEditForm
	view, err := s.updateViewLocked(r.Context(), pid, slug, func() (viewSettings, error) {
		if !form {
			return parseViewSettingsJSON(body)
		}
		var err error
		edit, err = parseViewSettingsForm(r.Header.Get("Content-Type"), body)
		if err != nil {
			return viewSettings{}, err
		}
		return viewSettings{title: &edit.Title, prompt: &edit.Prompt, version: edit.Version}, nil
	})
	if err != nil {
		var problem apiError
		if form && edit.Version != "" && errors.As(err, &problem) &&
			(problem.status == http.StatusBadRequest || problem.status == http.StatusConflict) {
			edit.API = r.URL.Path
			edit.ViewURL = "/" + namespace + "/views/" + slug
			edit.Error = problem.message
			edit.Conflict = problem.status == http.StatusConflict
			return renderViewEditError(w, r, uid, edit, problem.status)
		}
		return err
	}
	if form {
		target := "/" + namespace + "/views/" + view.Slug
		http.Redirect(w, r, target, http.StatusSeeOther)
		return nil
	}
	if view.Slug == wikiViewSlug {
		jsonOut(w, 200, viewDetail{viewDefinition: view})
		return nil
	}
	jsonOut(w, 200, view)
	return nil
}

type viewSettings struct {
	title, prompt *string
	version       string
}

func parseViewSettingsJSON(body requestBody) (viewSettings, error) {
	var req struct {
		Title  json.RawMessage `json:"title"`
		Prompt json.RawMessage `json:"prompt"`
	}
	if err := body.decode(&req); err != nil {
		return viewSettings{}, err
	}
	title, err := viewSettingText(req.Title)
	if err != nil {
		return viewSettings{}, err
	}
	prompt, err := viewSettingText(req.Prompt)
	return viewSettings{title: title, prompt: prompt}, err
}

func parseViewSettingsForm(contentType string, body requestBody) (viewEditForm, error) {
	if mediaType, _, _ := mime.ParseMediaType(contentType); mediaType != "application/x-www-form-urlencoded" {
		return viewEditForm{}, fail(415, "View settings form requires application/x-www-form-urlencoded")
	}
	if body.err != nil {
		return viewEditForm{}, fail(400, "View の設定フォームが大きすぎます")
	}
	values, err := url.ParseQuery(string(body.raw))
	if err != nil || len(values) != 3 || len(values["title"]) != 1 || len(values["prompt"]) != 1 ||
		len(values["version"]) != 1 || len(values.Get("version")) != 64 {
		return viewEditForm{}, fail(400, "invalid View settings form")
	}
	title, prompt := values.Get("title"), values.Get("prompt")
	if len(title)+len(prompt) > model.MaxJSONRequestBytes {
		return viewEditForm{}, fail(400, "View の設定は 16 MiB 以下にしてください")
	}
	edit := viewEditForm{Title: viewFormTitle.Replace(title), Prompt: viewFormPrompt.Replace(prompt), Version: values.Get("version")}
	if !validViewSetting(edit.Title) || !validViewSetting(edit.Prompt) {
		return edit, fail(400, "タイトルとプロンプトを入力してください")
	}
	return edit, nil
}

func viewSettingText(raw json.RawMessage) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || !validViewSetting(text) {
		return nil, fail(400, "view settings must contain text")
	}
	return &text, nil
}

func validViewSetting(text string) bool {
	return utf8.ValidString(text) && strings.TrimSpace(text) != ""
}

func (s *Server) deleteView(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	unlock, err := s.storage.lock(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.deleteViewLocked(r.Context(), pid, r.PathValue("view")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
