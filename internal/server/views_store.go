package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/prompts"
)

type viewPreset struct {
	Label, Title, Prompt string
}

var projectViewPresets = []viewPreset{
	{"プロジェクトのステータス", "Project map", "Wiki をもとに、プロジェクトの現在地、日程、次のマイルストーン、直近の判断、根拠を示す。"},
	{"カンバン", "カンバン", "Wiki の作業を状態別に並べ、担当と根拠を示す。"},
}

const wikiViewSlug = "wiki"

type viewDefinition struct {
	ID             string     `json:"view_id"`
	Slug           string     `json:"slug"`
	Title          string     `json:"title"`
	Prompt         string     `json:"prompt"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
	JobID          string     `json:"job_id,omitempty"`
	WikiSnapshotID string     `json:"wiki_snapshot_id,omitempty"`
	GeneratedAt    *time.Time `json:"generated_at,omitempty"`
}

func newWikiViewRow(pid string) store.ViewRow {
	return store.ViewRow{ProjectID: pid, ID: "vw_" + auth.Hash(pid + "/wiki")[:20], Slug: wikiViewSlug}
}

func (s *Server) defaultWikiPrompt() string {
	if s.Config.Prompts != nil {
		return s.Config.Prompts.WikiDefaultPrompt()
	}
	return prompts.DefaultWikiPrompt()
}

func (s *Server) wikiViewDefinition(row store.ViewRow, project model.Project) viewDefinition {
	view := viewDefinition{ID: row.ID, Slug: wikiViewSlug, Title: store.ValueString(row.Data.Title), Prompt: store.ValueString(row.Data.Prompt), CreatedAt: &project.Created, Status: "pending"}
	if view.Title == "" {
		view.Title = "Wiki"
	}
	if view.Prompt == "" {
		view.Prompt = s.defaultWikiPrompt()
	}
	if !project.WikiUpdatedAt.IsZero() {
		view.Status, view.GeneratedAt = "completed", &project.WikiUpdatedAt
	}
	return view
}

func sortViews(views []viewDefinition) {
	sort.Slice(views, func(i, j int) bool {
		a, b := views[i], views[j]
		if a.CreatedAt == nil {
			return b.CreatedAt == nil && a.Slug < b.Slug
		}
		if b.CreatedAt == nil {
			return true
		}
		if a.CreatedAt.Equal(*b.CreatedAt) {
			return a.ID < b.ID
		}
		return a.CreatedAt.Before(*b.CreatedAt)
	})
}

func (s *Server) viewProjection(row store.ViewRow, p model.Project) viewDefinition {
	if row.Slug == wikiViewSlug {
		return s.wikiViewDefinition(row, p)
	}
	v := viewDefinition{ID: row.ID, Slug: row.Slug, Title: store.ValueString(row.Data.Title), Prompt: store.ValueString(row.Data.Prompt), Status: store.ValueString(row.Data.Status), Error: store.ValueString(row.Data.Error), JobID: store.ValueString(row.Data.JobID), WikiSnapshotID: store.ValueString(row.Data.WikiSnapshotID)}
	if row.CreatedAt != nil {
		t := storageDate(*row.CreatedAt)
		v.CreatedAt = &t
	}
	if row.Data.GeneratedAt != nil {
		t := storageDate(*row.Data.GeneratedAt)
		v.GeneratedAt = &t
	}
	return v
}
func (s *Server) listViewsLocked(ctx context.Context, pid string) ([]viewDefinition, error) {
	views := []viewDefinition{}
	err := s.Store.Read(ctx, func(q *store.Queries) error {
		row, err := q.Project(pid)
		if err != nil {
			return err
		}
		if row.Deleting {
			return fail(404, "project not found")
		}
		p, err := projectProjection(q, row)
		if err != nil {
			return err
		}
		rows, err := q.ProjectViews(pid)
		if err != nil {
			return err
		}
		hasWiki := false
		for _, row := range rows {
			if row.Slug == wikiViewSlug {
				hasWiki = true
			}
			views = append(views, s.viewProjection(row, p))
		}
		if !hasWiki {
			views = append(views, s.wikiViewDefinition(newWikiViewRow(pid), p))
		}
		return nil
	})
	sortViews(views)
	return views, err
}
func (s *Server) viewBySlugLocked(ctx context.Context, pid, slug string) (viewDefinition, error) {
	var v viewDefinition
	err := s.Store.Read(ctx, func(q *store.Queries) error {
		p, err := q.Project(pid)
		if err != nil {
			return err
		}
		if p.Deleting {
			return fail(404, "project not found")
		}
		row, err := q.ViewBySlug(pid, slug)
		if errors.Is(err, sql.ErrNoRows) && slug == wikiViewSlug {
			row = newWikiViewRow(pid)
			err = nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "view not found")
		}
		if err != nil {
			return err
		}
		project, err := projectProjection(q, p)
		if err != nil {
			return err
		}
		v = s.viewProjection(row, project)
		return nil
	})
	return v, err
}
func (s *Server) createViewLocked(ctx context.Context, pid string, body requestBody) (viewDefinition, error) {
	var result viewDefinition
	err := s.write(ctx, func(q *store.Queries) error {
		var req viewCreateRequest
		if err := body.decode(&req); err != nil {
			return err
		}
		var slug string
		if req.Slug != nil {
			if err := json.Unmarshal(req.Slug, &slug); err != nil || !validSlug(slug) {
				return fail(400, "invalid view slug")
			}
		}
		title, prompt := req.Title, req.Prompt
		if !validViewSetting(title) || !validViewSetting(prompt) {
			return fail(400, "title and prompt must contain text")
		}
		p, err := q.Project(pid)
		if err != nil {
			return err
		}
		id := "vw_" + auth.Token()[:20]
		if slug == "" {
			slug = projectSlug(title)
			_, err = q.ViewBySlug(pid, slug)
			if err == nil || slug == wikiViewSlug {
				slug = "view-" + auth.Hash(id)[:20]
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if !validSlug(slug) {
			return fail(400, "invalid view slug")
		}
		_, err = q.ViewBySlug(pid, slug)
		if err == nil || slug == wikiViewSlug {
			return fail(409, "view slug is already in use")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := time.Now().UnixMilli()
		row := store.ViewRow{ID: id, ProjectID: pid, Slug: slug, CreatedAt: &now, Data: store.ViewData{Title: &title, Prompt: &prompt, Status: store.NullableString("pending")}}
		if err := q.PutView(row); err != nil {
			return err
		}
		project, err := projectProjection(q, p)
		if err != nil {
			return err
		}
		result = s.viewProjection(row, project)
		return nil
	})
	return result, err
}

// Compare the settings displayed in a browser form, independent of generation.
func viewSettingsVersion(view viewDefinition) string {
	return auth.Hash(fmt.Sprintf("%s:%d:%s%s", view.ID, len(view.Title), view.Title, view.Prompt))
}

func (s *Server) updateViewLocked(ctx context.Context, pid, slug string, settings func() (viewSettings, error)) (viewDefinition, error) {
	var result viewDefinition
	err := s.write(ctx, func(q *store.Queries) error {
		change, err := settings()
		if err != nil {
			return err
		}
		if change.title == nil && change.prompt == nil {
			return fail(400, "provide title or prompt with text")
		}
		row, err := q.ViewBySlug(pid, slug)
		if errors.Is(err, sql.ErrNoRows) && slug == wikiViewSlug {
			row = newWikiViewRow(pid)
			err = nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "view not found")
		}
		if err != nil {
			return err
		}
		p, err := q.Project(pid)
		if err != nil {
			return err
		}
		project, err := projectProjection(q, p)
		if err != nil {
			return err
		}
		if change.version != "" {
			current := s.viewProjection(row, project)
			if change.version != viewSettingsVersion(current) {
				return fail(409, "編集中に View の設定が更新されました")
			}
			// A form can display normalized text from an API write. Only
			// persist fields that differ from what the browser displayed.
			if change.title != nil && *change.title == viewFormTitle.Replace(current.Title) {
				change.title = nil
			}
			if change.prompt != nil && *change.prompt == viewFormPrompt.Replace(current.Prompt) {
				change.prompt = nil
			}
			if change.title == nil && change.prompt == nil {
				result = current
				return nil
			}
		}
		if change.title != nil {
			row.Data.Title = change.title
		}
		if change.prompt != nil {
			row.Data.Prompt = change.prompt
		}
		if err = q.PutView(row); err != nil {
			return err
		}
		result = s.viewProjection(row, project)
		return nil
	})
	return result, err
}
func (s *Server) deleteViewLocked(ctx context.Context, pid, slug string) error {
	return s.write(ctx, func(q *store.Queries) error {
		if slug == wikiViewSlug {
			return fail(409, "the wiki view cannot be deleted")
		}
		row, err := q.ViewBySlug(pid, slug)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "view not found")
		}
		if err != nil {
			return err
		}
		return q.DeleteView(pid, row.ID)
	})
}
func (s *Server) readViewBundleLocked(ctx context.Context, pid string, v viewDefinition) ([]byte, error) {
	if v.JobID == "" {
		return nil, fail(404, "view result not found")
	}
	var row store.ViewRow
	err := s.Store.Read(ctx, func(q *store.Queries) error {
		var err error
		row, err = q.View(pid, v.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "view result not found")
		}
		if err != nil {
			return err
		}
		if _, err := q.Project(pid); err != nil {
			return err
		}
		if row.BundleHash == nil || store.ValueString(row.Data.JobID) != v.JobID {
			return fail(404, "view result not found")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	body, err := s.Blobs.Get(ctx, model.ViewContentKey(pid, v.ID, model.ContentHash(*row.BundleHash)))
	if err == nil && string(model.HashContent(body)) != *row.BundleHash {
		return nil, errors.New("view bundle failed integrity check")
	}
	return body, err
}

func isNotFoundView(err error) bool {
	var problem apiError
	return errors.As(err, &problem) && problem.status == 404
}
