package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mash/kioku/internal/model"
)

type viewDetail struct {
	viewDefinition
	WikiUpdatedSinceRun *bool `json:"wiki_updated_since_run"`
}

func viewHasNewWiki(view viewDefinition, project model.Project) *bool {
	if view.GeneratedAt == nil {
		return nil
	}
	updated := view.WikiSnapshotID != completeViewWikiRevision(project)
	return &updated
}

type viewCreateRequest struct {
	Slug   json.RawMessage `json:"slug"`
	Title  string          `json:"title"`
	Prompt string          `json:"prompt"`
}

func (s *Server) createView(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	unlock, err := s.lockProject(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.checkOwner(pid, uid); err != nil {
		return err
	}
	var req viewCreateRequest
	if err := decode(w, r, &req); err != nil {
		return err
	}
	var slug string
	if req.Slug != nil {
		if err := json.Unmarshal(req.Slug, &slug); err != nil || !validSlug(slug) {
			return fail(400, "invalid view slug")
		}
	}
	view, err := s.createViewLocked(r.Context(), pid, slug, req.Title, req.Prompt)
	if err != nil {
		return err
	}
	jsonOut(w, 201, view)
	return nil
}

func (s *Server) getView(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	unlock, err := s.lockProject(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	var project model.Project
	err = s.Store.View(func(st model.State) error {
		var err error
		project, err = member(st, pid, uid)
		return err
	})
	if err != nil {
		return err
	}
	view, err := s.viewForDisplayLocked(r.Context(), project, r.PathValue("view"))
	if err != nil {
		return err
	}
	detail := viewDetail{viewDefinition: view}
	if view.Slug != wikiViewSlug {
		detail.WikiUpdatedSinceRun = viewHasNewWiki(view, project)
	}
	jsonOut(w, 200, detail)
	return nil
}

func (s *Server) updateView(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	unlock, err := s.lockProject(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.checkOwner(pid, uid); err != nil {
		return err
	}
	var req struct {
		Title  json.RawMessage `json:"title"`
		Prompt json.RawMessage `json:"prompt"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	title, err := viewSettingText(req.Title)
	if err != nil {
		return err
	}
	prompt, err := viewSettingText(req.Prompt)
	if err != nil {
		return err
	}
	if r.PathValue("view") == wikiViewSlug {
		view, err := s.updateWikiViewLocked(r.Context(), pid, title, prompt)
		if err != nil {
			return err
		}
		jsonOut(w, 200, viewDetail{viewDefinition: view})
		return nil
	}
	view, err := s.updateViewLocked(r.Context(), pid, r.PathValue("view"), title, prompt)
	if err != nil {
		return err
	}
	jsonOut(w, 200, view)
	return nil
}

func viewSettingText(raw json.RawMessage) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || strings.TrimSpace(text) == "" {
		return nil, fail(400, "view settings must contain text")
	}
	return &text, nil
}

func (s *Server) deleteView(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := r.PathValue("project")
	unlock, err := s.lockProject(r.Context(), pid)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.checkOwner(pid, uid); err != nil {
		return err
	}
	if r.PathValue("view") == wikiViewSlug {
		return fail(409, "the wiki view cannot be deleted")
	}
	if err := s.deleteViewLocked(r.Context(), pid, r.PathValue("view")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
