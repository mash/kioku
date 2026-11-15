package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
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
	Version        string     `json:"-"`
}

type viewIndex struct {
	Wiki    *viewIndexEntry           `json:"-"`
	Views   map[string]viewIndexEntry `json:"views"`
	Removed []string                  `json:"removed,omitempty"`
}

type viewIndexEntry struct {
	ID      string `json:"id"`
	Version string `json:"settings_generation"`
	Result  string `json:"result_job_id,omitempty"`
	Hash    string `json:"bundle_hash,omitempty"`
	Epoch   string `json:"view_epoch,omitempty"`
}

type viewResult struct {
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
	JobID          string     `json:"job_id,omitempty"`
	WikiSnapshotID string     `json:"wiki_snapshot_id,omitempty"`
	GeneratedAt    *time.Time `json:"generated_at,omitempty"`
}

type viewPublication struct {
	Prior     viewResult `json:"prior"`
	Next      viewResult `json:"next"`
	PriorHash string     `json:"prior_hash,omitempty"`
	NextHash  string     `json:"next_hash,omitempty"`
}

type viewEdit struct {
	Version string `json:"settings_generation"`
	Title   string `json:"title"`
	Prompt  string `json:"prompt"`
}

func (index *viewIndex) UnmarshalJSON(body []byte) error {
	type plain viewIndex
	var decoded plain
	if err := json.Unmarshal(body, &decoded); err != nil {
		return err
	}
	*index = viewIndex(decoded)
	if entry, ok := index.Views[wikiViewSlug]; ok {
		index.Wiki = &entry
		delete(index.Views, wikiViewSlug)
	}
	return nil
}

func (index viewIndex) MarshalJSON() ([]byte, error) {
	type plain viewIndex
	encoded := plain(index)
	encoded.Views = make(map[string]viewIndexEntry, len(index.Views)+1)
	for slug, entry := range index.Views {
		encoded.Views[slug] = entry
	}
	if index.Wiki != nil {
		encoded.Views[wikiViewSlug] = *index.Wiki
	}
	return json.Marshal(encoded)
}

func newWikiViewRecord(pid string) wikiViewRecord {
	return wikiViewRecord{ID: "vw_" + auth.Hash(pid + "/wiki")[:20], Slug: wikiViewSlug}
}

func (s *Server) readWikiViewLocked(ctx context.Context, pid string) (wikiViewRecord, error) {
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return wikiViewRecord{}, err
	}
	return s.loadWikiViewLocked(ctx, pid, index)
}

func (s *Server) loadWikiViewLocked(ctx context.Context, pid string, index viewIndex) (wikiViewRecord, error) {
	expected := newWikiViewRecord(pid)
	if index.Wiki == nil {
		return expected, nil
	}
	var record wikiViewRecord
	if err := s.readViewRecord(ctx, viewKey(pid, index.Wiki.ID, "view.json"), &record); err != nil {
		return record, err
	}
	if index.Wiki.ID != expected.ID || record.ID != expected.ID || record.Slug != expected.Slug {
		return record, errors.New("wiki view metadata does not match project index")
	}
	return record, nil
}

func (s *Server) wikiViewDefinition(record wikiViewRecord, project model.Project) viewDefinition {
	view := viewDefinition{ID: record.ID, Slug: wikiViewSlug, Title: record.Title, Prompt: record.Prompt, CreatedAt: &project.Created, Status: "pending"}
	if view.Title == "" {
		view.Title = "Wiki"
	}
	if view.Prompt == "" {
		if s.Config.Prompts != nil {
			view.Prompt = s.Config.Prompts.WikiDefaultPrompt()
		} else {
			view.Prompt = prompts.DefaultWikiPrompt()
		}
	}
	if !project.WikiIncomplete && !project.WikiUpdatedAt.IsZero() {
		view.Status, view.GeneratedAt = "completed", &project.WikiUpdatedAt
	}
	return view
}

func (s *Server) updateWikiViewLocked(ctx context.Context, pid string, title, prompt *string) (viewDefinition, error) {
	if title == nil && prompt == nil {
		return viewDefinition{}, fail(400, "provide title or prompt with text")
	}
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return viewDefinition{}, err
	}
	// A missing or corrupt record is replaced so the owner can repair it.
	// Storage errors still fail, so a transient read cannot discard settings.
	record, err := s.loadWikiViewLocked(ctx, pid, index)
	if err != nil {
		if _, getErr := s.Blobs.Get(ctx, viewKey(pid, index.Wiki.ID, "view.json")); getErr != nil && !viewObjectMissing(getErr) {
			return viewDefinition{}, getErr
		}
		record = newWikiViewRecord(pid)
	}
	prior := record
	if title != nil {
		record.Title = *title
	}
	if prompt != nil {
		record.Prompt = *prompt
	}
	if err := s.writeViewRecord(ctx, viewKey(pid, record.ID, "view.json"), record); err != nil {
		return viewDefinition{}, err
	}
	if index.Wiki == nil || record != prior {
		index.Wiki = &viewIndexEntry{ID: record.ID, Version: auth.Token()[:20]}
		if err := s.writeViewRecord(ctx, viewsKey(pid, "index.json"), index); err != nil {
			return viewDefinition{}, err
		}
	}
	project, err := s.viewProjectLocked(pid)
	if err != nil {
		return viewDefinition{}, err
	}
	return s.wikiViewDefinition(record, project), nil
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

func (v viewDefinition) result() viewResult {
	return viewResult{v.Status, v.Error, v.JobID, v.WikiSnapshotID, v.GeneratedAt}
}

func (v *viewDefinition) applyResult(result viewResult) {
	v.Status, v.Error = result.Status, result.Error
	v.JobID, v.WikiSnapshotID, v.GeneratedAt = result.JobID, result.WikiSnapshotID, result.GeneratedAt
}

const maxViewRecordBytes = 16 << 20

func viewsKey(pid string, suffix string) blob.Key {
	return blob.Key("projects/" + pid + "/.kioku/views/" + suffix)
}

func viewKey(pid, id, name string) blob.Key {
	return viewsKey(pid, id+"/"+name)
}

func viewObjectMissing(err error) bool {
	var missing interface{ NotFound() bool }
	return errors.Is(err, fs.ErrNotExist) || errors.As(err, &missing) && missing.NotFound()
}

func (s *Server) readViewRecord(ctx context.Context, key blob.Key, value any) error {
	body, err := s.Blobs.Get(ctx, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, value)
}

func encodeViewRecord(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(body) > maxViewRecordBytes {
		return nil, fail(400, "view record exceeds storage limit")
	}
	return body, nil
}

func (s *Server) writeViewRecord(ctx context.Context, key blob.Key, value any) error {
	body, err := encodeViewRecord(value)
	if err != nil {
		return err
	}
	return s.Blobs.Put(ctx, key, body)
}

func (s *Server) readViewIndexLocked(ctx context.Context, pid string) (viewIndex, error) {
	var index viewIndex
	err := s.readViewRecord(ctx, viewsKey(pid, "index.json"), &index)
	if !viewObjectMissing(err) {
		if err == nil && index.Views == nil {
			index.Views = map[string]viewIndexEntry{}
		}
		return index, err
	}
	// The index also records that the default was initialized, including after
	// the user deletes it. A retry of the initial write uses the same ID.
	id := "vw_" + auth.Hash(pid + "/project-map")[:20]
	preset := projectViewPresets[0]
	now := time.Now().UTC()
	view := viewDefinition{CreatedAt: &now, ID: id, Slug: "project-map", Title: preset.Title, Prompt: preset.Prompt, Status: "pending"}
	if err := s.writeViewRecord(ctx, viewKey(pid, id, "view.json"), view); err != nil {
		return index, err
	}
	index.Views = map[string]viewIndexEntry{"project-map": {ID: id, Version: auth.Token()[:20]}}
	return index, s.writeViewRecord(ctx, viewsKey(pid, "index.json"), index)
}

func (s *Server) seedViewsLocked(ctx context.Context, pid string) error {
	_, err := s.readViewIndexLocked(ctx, pid)
	return err
}

func (s *Server) viewProjectLocked(pid string) (model.Project, error) {
	var p model.Project
	err := s.Store.View(func(st model.State) error {
		var ok bool
		p, ok = st.Projects[pid]
		if !ok || p.Deleting {
			return fail(404, "project not found")
		}
		return nil
	})
	return p, err
}

func completeViewWikiRevision(project model.Project) string {
	if project.WikiCompleteRevision != "" {
		return project.WikiCompleteRevision
	}
	if !project.WikiIncomplete {
		return project.Revision
	}
	return ""
}

// Callers hold the project lock for authorization, lookup, and the associated
// read or write. The index defines visibility and commits a published result.
func (s *Server) listViewsLocked(ctx context.Context, pid string) ([]viewDefinition, error) {
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return nil, err
	}
	project, err := s.viewProjectLocked(pid)
	if err != nil {
		return nil, err
	}
	if err := s.collectDeletedViewsLocked(ctx, pid, index); err != nil {
		slog.Warn("view cleanup deferred", "project", pid, "error", err)
	}
	views, err := s.readViewsLocked(ctx, pid, project.ViewEpoch, index)
	if err != nil {
		return nil, err
	}
	wiki, err := s.loadWikiViewLocked(ctx, pid, index)
	if err != nil {
		return nil, err
	}
	views = append(views, s.wikiViewDefinition(wiki, project))
	sortViews(views)
	return views, nil
}

func (s *Server) readViewsLocked(ctx context.Context, pid, epoch string, index viewIndex) ([]viewDefinition, error) {
	views := make([]viewDefinition, 0, len(index.Views))
	for slug, entry := range index.Views {
		view, err := s.loadViewLocked(ctx, pid, slug, epoch, entry)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Server) viewBySlugLocked(ctx context.Context, pid, slug string) (viewDefinition, error) {
	return s.findViewLocked(ctx, pid, slug, "")
}

func (s *Server) viewByIDLocked(ctx context.Context, pid, id string) (viewDefinition, error) {
	return s.findViewLocked(ctx, pid, "", id)
}

func (s *Server) findViewLocked(ctx context.Context, pid, slug, id string) (viewDefinition, error) {
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return viewDefinition{}, err
	}
	if id != "" {
		for name, entry := range index.Views {
			if entry.ID == id {
				slug = name
				break
			}
		}
	}
	entry, exists := index.Views[slug]
	if !exists {
		return viewDefinition{}, fail(404, "view not found")
	}
	project, err := s.viewProjectLocked(pid)
	if err != nil {
		return viewDefinition{}, err
	}
	return s.loadViewLocked(ctx, pid, slug, project.ViewEpoch, entry)
}

func (s *Server) loadViewLocked(ctx context.Context, pid, slug, epoch string, entry viewIndexEntry) (viewDefinition, error) {
	var view viewDefinition
	if err := s.recoverViewPublicationLocked(ctx, pid, entry); err != nil {
		return view, err
	}
	err := s.readViewRecord(ctx, viewKey(pid, entry.ID, "view.json"), &view)
	if err != nil {
		return view, err
	}
	if view.ID != entry.ID || view.Slug != slug || view.JobID != entry.Result {
		return view, errors.New("view metadata does not match project index")
	}
	if err := s.recoverViewEditLocked(ctx, pid, entry.Version, &view); err != nil {
		return view, err
	}
	if entry.Result != "" && entry.Epoch != epoch {
		view.clearResult()
	}
	view.Version = entry.Version
	return view, nil
}

func (v *viewDefinition) clearResult() {
	v.JobID, v.WikiSnapshotID, v.GeneratedAt = "", "", nil
}

func (s *Server) createViewLocked(ctx context.Context, pid, slug, title, prompt string) (viewDefinition, error) {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(prompt) == "" {
		return viewDefinition{}, fail(400, "title and prompt must contain text")
	}
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return viewDefinition{}, err
	}
	id := "vw_" + auth.Token()[:20]
	if slug == "" {
		slug = projectSlug(title)
		if _, exists := index.Views[slug]; exists || slug == wikiViewSlug {
			slug = "view-" + auth.Hash(id)[:20]
		}
	}
	if !validSlug(slug) {
		return viewDefinition{}, fail(400, "invalid view slug")
	}
	if _, exists := index.Views[slug]; exists || slug == wikiViewSlug {
		return viewDefinition{}, fail(409, "view slug is already in use")
	}
	now := time.Now().UTC()
	view := viewDefinition{CreatedAt: &now, ID: id, Slug: slug, Title: title, Prompt: prompt, Status: "pending", Version: auth.Token()[:20]}
	if err = s.writeViewRecord(ctx, viewKey(pid, id, "view.json"), view); err != nil {
		return viewDefinition{}, err
	}
	index.Views[slug] = viewIndexEntry{ID: id, Version: view.Version}
	return view, s.writeViewRecord(ctx, viewsKey(pid, "index.json"), index)
}

func (s *Server) updateViewLocked(ctx context.Context, pid, slug string, title, prompt *string) (viewDefinition, error) {
	if (title == nil && prompt == nil) || (title != nil && strings.TrimSpace(*title) == "") || (prompt != nil && strings.TrimSpace(*prompt) == "") {
		return viewDefinition{}, fail(400, "provide title or prompt with text")
	}
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return viewDefinition{}, err
	}
	entry, exists := index.Views[slug]
	if !exists {
		return viewDefinition{}, fail(404, "view not found")
	}
	// Load the on-disk result fields even when its epoch is obsolete. Only
	// invalidation changes those fields and their corresponding index values.
	view, err := s.loadViewLocked(ctx, pid, slug, entry.Epoch, entry)
	if err != nil {
		return viewDefinition{}, err
	}
	edit := viewEdit{Version: auth.Token()[:20], Title: view.Title, Prompt: view.Prompt}
	if title != nil {
		edit.Title = *title
	}
	if prompt != nil {
		edit.Prompt = *prompt
	}
	if view.applyEdit(edit) {
		if err := s.commitViewEditLocked(ctx, pid, index, entry, edit, view); err != nil {
			return viewDefinition{}, err
		}
		view.Version = edit.Version
	}
	project, err := s.viewProjectLocked(pid)
	if entry.Result != "" && entry.Epoch != project.ViewEpoch {
		view.clearResult()
	}
	return view, err
}

func (v *viewDefinition) applyEdit(edit viewEdit) bool {
	if v.Title == edit.Title && v.Prompt == edit.Prompt {
		return false
	}
	v.Title, v.Prompt = edit.Title, edit.Prompt
	if v.Status == "running" {
		v.Status, v.Error = "skipped", "設定が変更された"
	}
	return true
}

func (s *Server) commitViewEditLocked(ctx context.Context, pid string, index viewIndex, entry viewIndexEntry, edit viewEdit, view viewDefinition) error {
	body, err := encodeViewRecord(view)
	if err != nil {
		return err
	}
	if err := s.writeViewRecord(ctx, viewKey(pid, view.ID, "edit.json"), edit); err != nil {
		return err
	}
	entry.Version = edit.Version
	index.Views[view.Slug] = entry
	// The index changes the settings generation atomically. Recovery applies
	// the matching edit before another operation can read or use those settings.
	if err = s.writeViewRecord(ctx, viewsKey(pid, "index.json"), index); err != nil {
		return err
	}
	if err := s.Blobs.Put(ctx, viewKey(pid, view.ID, "view.json"), body); err != nil {
		return err
	}
	s.clearViewEditLocked(ctx, pid, view.ID)
	return nil
}

func (s *Server) recoverViewEditLocked(ctx context.Context, pid, version string, view *viewDefinition) error {
	var edit viewEdit
	err := s.readViewRecord(ctx, viewKey(pid, view.ID, "edit.json"), &edit)
	if viewObjectMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if edit.Version == version && view.applyEdit(edit) {
		if err := s.writeViewRecord(ctx, viewKey(pid, view.ID, "view.json"), view); err != nil {
			return err
		}
	}
	s.clearViewEditLocked(ctx, pid, view.ID)
	return nil
}

func (s *Server) clearViewEditLocked(ctx context.Context, pid, id string) {
	if err := s.Blobs.Delete(ctx, viewKey(pid, id, "edit.json")); err != nil && !viewObjectMissing(err) {
		slog.Warn("view edit cleanup deferred", "project", pid, "error", err)
	}
}

func (s *Server) deleteViewLocked(ctx context.Context, pid, slug string) error {
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return err
	}
	entry, exists := index.Views[slug]
	if !exists {
		return fail(404, "view not found")
	}
	delete(index.Views, slug)
	index.Removed = append(index.Removed, entry.ID)
	if err := s.writeViewRecord(ctx, viewsKey(pid, "index.json"), index); err != nil {
		return err
	}
	if err := s.collectDeletedViewsLocked(ctx, pid, index); err != nil {
		slog.Warn("view cleanup deferred", "project", pid, "error", err)
	}
	return nil
}

func (s *Server) collectDeletedViewsLocked(ctx context.Context, pid string, index viewIndex) error {
	if len(index.Removed) == 0 {
		return nil
	}
	for _, id := range index.Removed {
		if err := s.deleteViewFilesLocked(ctx, pid, id); err != nil {
			return err
		}
	}
	index.Removed = nil
	return s.writeViewRecord(ctx, viewsKey(pid, "index.json"), index)
}

func (s *Server) deleteViewFilesLocked(ctx context.Context, pid, id string) error {
	for _, name := range []string{"bundle.zip", "view.json", "edit.json", "publish.json", "previous.zip", "candidate.zip"} {
		if err := s.Blobs.Delete(ctx, viewKey(pid, id, name)); err != nil && !viewObjectMissing(err) {
			return err
		}
	}
	return nil
}

func (s *Server) setViewStatusLocked(ctx context.Context, pid, id, version, status, reason string) (bool, error) {
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return false, err
	}
	for slug, entry := range index.Views {
		if entry.ID != id {
			continue
		}
		if entry.Version != version {
			return false, nil
		}
		view, err := s.loadViewLocked(ctx, pid, slug, entry.Epoch, entry)
		if err != nil {
			return false, err
		}
		view.Status, view.Error = status, reason
		return true, s.writeViewRecord(ctx, viewKey(pid, id, "view.json"), view)
	}
	return false, nil
}

func (s *Server) publishViewLocked(ctx context.Context, pid, id, version, jobID, wikiSnapshotID string, bundle []byte) (bool, error) {
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return false, err
	}
	project, err := s.viewProjectLocked(pid)
	if err != nil {
		return false, err
	}
	if wikiSnapshotID == "" || project.WikiIncomplete || completeViewWikiRevision(project) != wikiSnapshotID {
		return false, nil
	}
	for slug, entry := range index.Views {
		if entry.ID != id {
			continue
		}
		if entry.Result == jobID {
			return true, s.recoverViewPublicationLocked(ctx, pid, entry)
		}
		if entry.Version != version {
			return false, nil
		}
		prior, err := s.loadViewLocked(ctx, pid, slug, entry.Epoch, entry)
		if err != nil {
			return false, err
		}
		next := prior
		now := time.Now().UTC()
		next.Status, next.Error = "completed", ""
		next.JobID, next.WikiSnapshotID, next.GeneratedAt = jobID, wikiSnapshotID, &now
		hash := auth.Hash(string(bundle))
		record := viewPublication{Prior: prior.result(), Next: next.result(), PriorHash: entry.Hash, NextHash: hash}
		if err = s.stageViewPublicationLocked(ctx, pid, entry, record, bundle); err != nil {
			return false, err
		}
		entry.Result, entry.Hash, entry.Epoch = jobID, hash, project.ViewEpoch
		index.Views[slug] = entry
		return true, s.commitViewPublicationLocked(ctx, pid, index, entry, next, bundle)
	}
	return false, nil
}

func (s *Server) stageViewPublicationLocked(ctx context.Context, pid string, entry viewIndexEntry, record viewPublication, bundle []byte) error {
	id := entry.ID
	if entry.Result != "" {
		old, err := s.Blobs.Get(ctx, viewKey(pid, id, "bundle.zip"))
		if err != nil {
			return err
		}
		if auth.Hash(string(old)) != entry.Hash {
			return errors.New("previous view bundle failed integrity check")
		}
		if err = s.Blobs.Put(ctx, viewKey(pid, id, "previous.zip"), old); err != nil {
			return err
		}
	}
	if err := s.Blobs.Put(ctx, viewKey(pid, id, "candidate.zip"), bundle); err != nil {
		return err
	}
	return s.writeViewRecord(ctx, viewKey(pid, id, "publish.json"), record)
}

func (s *Server) commitViewPublicationLocked(ctx context.Context, pid string, index viewIndex, entry viewIndexEntry, next viewDefinition, bundle []byte) error {
	id := entry.ID
	if err := s.Blobs.Put(ctx, viewKey(pid, id, "bundle.zip"), bundle); err != nil {
		return err
	}
	if err := s.writeViewRecord(ctx, viewKey(pid, id, "view.json"), next); err != nil {
		return err
	}
	if err := s.writeViewRecord(ctx, viewsKey(pid, "index.json"), index); err != nil {
		return err
	}
	s.clearViewPublicationLocked(ctx, pid, id)
	return nil
}

func (s *Server) recoverViewPublicationLocked(ctx context.Context, pid string, entry viewIndexEntry) error {
	id := entry.ID
	var record viewPublication
	err := s.readViewRecord(ctx, viewKey(pid, id, "publish.json"), &record)
	if viewObjectMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	result, backup, hash := record.Prior, "previous.zip", record.PriorHash
	if entry.Result == record.Next.JobID {
		result, backup, hash = record.Next, "candidate.zip", record.NextHash
	}
	if entry.Result != result.JobID || entry.Hash != hash {
		return errors.New("view index and publication record disagree")
	}
	metaKey := viewKey(pid, id, "view.json")
	var view viewDefinition
	if err := s.readViewRecord(ctx, metaKey, &view); err != nil {
		return err
	}
	// Settings writes and deletion recover under the same lock first. Only
	// restore result fields, so recovery cannot revert newer configuration.
	if view.JobID != result.JobID {
		view.applyResult(result)
		if err := s.writeViewRecord(ctx, metaKey, view); err != nil {
			return err
		}
	}
	if err := s.restoreViewBundleLocked(ctx, pid, entry, backup); err != nil {
		if entry.Result != "" {
			return err
		}
		// The index already prevents reads of this ZIP. Keep the record so
		// later recovery can retry deleting the unreferenced object.
		slog.Warn("view bundle deletion deferred", "project", pid, "error", err)
		return nil
	}
	s.clearViewPublicationLocked(ctx, pid, id)
	return nil
}

func (s *Server) restoreViewBundleLocked(ctx context.Context, pid string, entry viewIndexEntry, backup string) error {
	key := viewKey(pid, entry.ID, "bundle.zip")
	if entry.Result == "" {
		err := s.Blobs.Delete(ctx, key)
		if viewObjectMissing(err) {
			return nil
		}
		return err
	}
	current, err := s.Blobs.Get(ctx, key)
	if err == nil && auth.Hash(string(current)) == entry.Hash {
		return nil
	}
	body, err := s.Blobs.Get(ctx, viewKey(pid, entry.ID, backup))
	if err != nil {
		return err
	}
	if auth.Hash(string(body)) != entry.Hash {
		return errors.New("view recovery bundle failed integrity check")
	}
	return s.Blobs.Put(ctx, key, body)
}

func (s *Server) clearViewPublicationLocked(ctx context.Context, pid, id string) {
	if err := s.removeViewPublicationLocked(ctx, pid, id); err != nil {
		slog.Warn("view publication cleanup deferred", "project", pid, "error", err)
	}
}

func (s *Server) removeViewPublicationLocked(ctx context.Context, pid, id string) error {
	// Once the record is gone there is nothing to recover, even if deleting
	// a staging copy fails. Later attempts can overwrite the staging names.
	if err := s.Blobs.Delete(ctx, viewKey(pid, id, "publish.json")); err != nil && !viewObjectMissing(err) {
		return err
	}
	for _, name := range []string{"previous.zip", "candidate.zip"} {
		if err := s.Blobs.Delete(ctx, viewKey(pid, id, name)); err != nil && !viewObjectMissing(err) {
			slog.Warn("view staging cleanup deferred", "project", pid, "error", err)
		}
	}
	return nil
}

func (s *Server) readViewBundleLocked(ctx context.Context, pid string, view viewDefinition) ([]byte, error) {
	if view.JobID == "" {
		return nil, fail(404, "view result not found")
	}
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return nil, err
	}
	project, err := s.viewProjectLocked(pid)
	if err != nil {
		return nil, err
	}
	entry := index.Views[view.Slug]
	if entry.Result != view.JobID || entry.ID != view.ID || entry.Epoch != project.ViewEpoch {
		return nil, fail(404, "view result not found")
	}
	if err := s.recoverViewPublicationLocked(ctx, pid, entry); err != nil {
		return nil, err
	}
	body, err := s.Blobs.Get(ctx, viewKey(pid, view.ID, "bundle.zip"))
	if err == nil && auth.Hash(string(body)) != entry.Hash {
		return nil, errors.New("view bundle failed integrity check")
	}
	return body, err
}

func (s *Server) invalidateViewsLocked(ctx context.Context, pid string) error {
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return err
	}
	project, err := s.viewProjectLocked(pid)
	if err != nil {
		return err
	}
	for slug, entry := range index.Views {
		if entry.Result == "" || entry.Epoch == project.ViewEpoch {
			continue
		}
		if err := s.invalidateViewResultLocked(ctx, pid, slug, &index, entry); err != nil {
			return err
		}
	}
	return s.collectDeletedViewsLocked(ctx, pid, index)
}

func (s *Server) invalidateViewResultLocked(ctx context.Context, pid, slug string, index *viewIndex, entry viewIndexEntry) error {
	prior, err := s.loadViewLocked(ctx, pid, slug, entry.Epoch, entry)
	if err != nil {
		return err
	}
	next := prior
	next.clearResult()
	record := viewPublication{Prior: prior.result(), Next: next.result(), PriorHash: entry.Hash}
	if err := s.writeViewRecord(ctx, viewKey(pid, entry.ID, "publish.json"), record); err != nil {
		return err
	}
	if err := s.writeViewRecord(ctx, viewKey(pid, entry.ID, "view.json"), next); err != nil {
		return err
	}
	entry.Result, entry.Hash, entry.Epoch = "", "", ""
	index.Views[slug] = entry
	if err := s.writeViewRecord(ctx, viewsKey(pid, "index.json"), *index); err != nil {
		return err
	}
	return s.recoverViewPublicationLocked(ctx, pid, entry)
}

func (s *Server) collectViewsLocked(ctx context.Context, pid string) error {
	if err := s.invalidateViewsLocked(ctx, pid); err != nil {
		return err
	}
	index, err := s.readViewIndexLocked(ctx, pid)
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(index.Views)+1)
	if index.Wiki != nil {
		live[index.Wiki.ID] = true
	}
	project, err := s.viewProjectLocked(pid)
	if err != nil {
		return err
	}
	for slug, entry := range index.Views {
		live[entry.ID] = true
		if _, err := s.loadViewLocked(ctx, pid, slug, project.ViewEpoch, entry); err != nil {
			return err
		}
	}
	prefix := string(viewsKey(pid, ""))
	keys, err := s.Blobs.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		suffix, inProject := strings.CutPrefix(string(key), prefix)
		id, name, found := strings.Cut(suffix, "/")
		if !inProject || !found || !strings.HasPrefix(id, "vw_") {
			continue
		}
		if live[id] {
			if name != "previous.zip" && name != "candidate.zip" {
				continue
			}
			_, err := s.Blobs.Get(ctx, viewKey(pid, id, "publish.json"))
			if !viewObjectMissing(err) {
				continue
			}
		}
		if err := s.Blobs.Delete(ctx, key); err != nil && !viewObjectMissing(err) {
			return err
		}
	}
	return nil
}
