package server

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
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

type wikiViewRecord struct {
	ID     string `json:"view_id"`
	Slug   string `json:"slug"`
	Title  string `json:"title,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

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

func newWikiViewRecord(pid string) wikiViewRecord {
	return wikiViewRecord{ID: "vw_" + auth.Hash(pid + "/wiki")[:20], Slug: wikiViewSlug}
}

func (s *Server) defaultWikiPrompt() string {
	if s.Config.Prompts != nil {
		return s.Config.Prompts.WikiDefaultPrompt()
	}
	return prompts.DefaultWikiPrompt()
}

func (s *Server) wikiViewDefinition(record wikiViewRecord, project model.Project) viewDefinition {
	view := viewDefinition{ID: record.ID, Slug: wikiViewSlug, Title: record.Title, Prompt: record.Prompt, CreatedAt: &project.Created, Status: "pending"}
	if view.Title == "" {
		view.Title = "Wiki"
	}
	if view.Prompt == "" {
		view.Prompt = s.defaultWikiPrompt()
	}
	if !project.WikiIncomplete && !project.WikiUpdatedAt.IsZero() {
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

func completeViewWikiRevision(p model.Project) string { return p.WikiCompleteRevision }
func viewProjection(row store.ViewRow, p model.Project, s *Server) viewDefinition {
	if row.Slug == wikiViewSlug {
		return s.wikiViewDefinition(wikiViewRecord{ID: row.ID, Slug: row.Slug, Title: valueString(row.Data.Title), Prompt: valueString(row.Data.Prompt)}, p)
	}
	v := viewDefinition{ID: row.ID, Slug: row.Slug, Title: valueString(row.Data.Title), Prompt: valueString(row.Data.Prompt), Status: valueString(row.Data.Status), Error: valueString(row.Data.Error), JobID: valueString(row.Data.JobID), WikiSnapshotID: valueString(row.Data.WikiSnapshotID)}
	if row.CreatedAt != nil {
		t := storageDate(*row.CreatedAt)
		v.CreatedAt = &t
	}
	if row.Data.GeneratedAt != nil {
		t := storageDate(*row.Data.GeneratedAt)
		v.GeneratedAt = &t
	}
	if row.BundleHash != nil && valueString(row.ViewEpoch) != p.ViewEpoch {
		v.JobID = ""
		v.WikiSnapshotID = ""
		v.GeneratedAt = nil
	}
	return v
}
func (s *Server) readWikiViewLocked(ctx context.Context, pid string) (wikiViewRecord, error) {
	var record wikiViewRecord
	err := s.Store.Read(ctx, func(q *store.Queries) error {
		row, err := q.ViewBySlug(pid, wikiViewSlug)
		if errors.Is(err, sql.ErrNoRows) {
			record = newWikiViewRecord(pid)
			return nil
		}
		if err != nil {
			return err
		}
		record = wikiViewRecord{ID: row.ID, Slug: row.Slug, Title: valueString(row.Data.Title), Prompt: valueString(row.Data.Prompt)}
		return nil
	})
	return record, err
}
func (s *Server) viewProjectLocked(pid string) (model.Project, error) {
	var p model.Project
	err := s.Store.Read(context.Background(), func(q *store.Queries) error {
		row, err := q.Project(pid)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "project not found")
		}
		if err != nil {
			return err
		}
		if row.Deleting {
			return fail(404, "project not found")
		}
		p, err = projectProjection(q, row)
		return err
	})
	return p, err
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
			views = append(views, viewProjection(row, p, s))
		}
		if !hasWiki {
			views = append(views, s.wikiViewDefinition(newWikiViewRecord(pid), p))
		}
		return nil
	})
	sortViews(views)
	return views, err
}
func (s *Server) viewBySlugLocked(ctx context.Context, pid, slug string) (viewDefinition, error) {
	return s.findViewLocked(ctx, pid, slug, "")
}
func (s *Server) viewByIDLocked(ctx context.Context, pid, id string) (viewDefinition, error) {
	return s.findViewLocked(ctx, pid, "", id)
}
func (s *Server) viewForDisplayLocked(ctx context.Context, p model.Project, slug string) (viewDefinition, error) {
	return s.viewBySlugLocked(ctx, p.ID, slug)
}
func (s *Server) findViewLocked(ctx context.Context, pid, slug, id string) (viewDefinition, error) {
	var v viewDefinition
	err := s.Store.Read(ctx, func(q *store.Queries) error {
		p, err := q.Project(pid)
		if err != nil {
			return err
		}
		if p.Deleting {
			return fail(404, "project not found")
		}
		var row store.ViewRow
		if id != "" {
			row, err = q.View(pid, id)
		} else {
			row, err = q.ViewBySlug(pid, slug)
		}
		if errors.Is(err, sql.ErrNoRows) && slug == wikiViewSlug {
			row = store.ViewRow{ProjectID: pid, ID: newWikiViewRecord(pid).ID, Slug: wikiViewSlug}
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
		v = viewProjection(row, project, s)
		return nil
	})
	return v, err
}
func (s *Server) createViewLocked(ctx context.Context, pid, slug, title, prompt string) (viewDefinition, error) {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(prompt) == "" {
		return viewDefinition{}, fail(400, "title and prompt must contain text")
	}
	var result viewDefinition
	err := s.write(ctx, func(q *store.Queries) error {
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
		row := store.ViewRow{ID: id, ProjectID: pid, Slug: slug, CreatedAt: &now, Data: store.ViewData{Title: &title, Prompt: &prompt, Status: nullableString("pending")}}
		if err := q.PutView(row); err != nil {
			return err
		}
		project, err := projectProjection(q, p)
		if err != nil {
			return err
		}
		result = viewProjection(row, project, s)
		return nil
	})
	return result, err
}
func (s *Server) updateWikiViewLocked(ctx context.Context, pid string, title, prompt *string) (viewDefinition, error) {
	return s.updateViewLocked(ctx, pid, wikiViewSlug, title, prompt)
}
func (s *Server) updateViewLocked(ctx context.Context, pid, slug string, title, prompt *string) (viewDefinition, error) {
	if title == nil && prompt == nil || title != nil && strings.TrimSpace(*title) == "" || prompt != nil && strings.TrimSpace(*prompt) == "" {
		return viewDefinition{}, fail(400, "provide title or prompt with text")
	}
	var result viewDefinition
	err := s.write(ctx, func(q *store.Queries) error {
		row, err := q.ViewBySlug(pid, slug)
		if errors.Is(err, sql.ErrNoRows) && slug == wikiViewSlug {
			row = store.ViewRow{ProjectID: pid, ID: newWikiViewRecord(pid).ID, Slug: wikiViewSlug}
			err = nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "view not found")
		}
		if err != nil {
			return err
		}
		if title != nil {
			row.Data.Title = title
		}
		if prompt != nil {
			row.Data.Prompt = prompt
		}
		if err = q.PutView(row); err != nil {
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
		result = viewProjection(row, project, s)
		return nil
	})
	return result, err
}
func (s *Server) deleteViewLocked(ctx context.Context, pid, slug string) error {
	if slug == wikiViewSlug {
		return fail(409, "the wiki view cannot be deleted")
	}
	return s.write(ctx, func(q *store.Queries) error {
		row, err := q.ViewBySlug(pid, slug)
		if errors.Is(err, sql.ErrNoRows) && slug == wikiViewSlug {
			row = store.ViewRow{ProjectID: pid, ID: newWikiViewRecord(pid).ID, Slug: wikiViewSlug}
			err = nil
		}
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
		p, err := q.Project(pid)
		if err != nil {
			return err
		}
		if row.BundleHash == nil || valueString(row.Data.JobID) != v.JobID || !equalOptional(row.ViewEpoch, p.ViewEpoch) {
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
