package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

type viewFaultStore struct {
	blob.Store
	putSuffix  string
	putApplied bool
	getKey     blob.Key
	getOnce    bool
	deleteKey  blob.Key
}

func (s *viewFaultStore) Get(ctx context.Context, key blob.Key) ([]byte, error) {
	if key == s.getKey {
		if s.getOnce {
			s.getKey = ""
		}
		return nil, errors.New("test get interrupted")
	}
	return s.Store.Get(ctx, key)
}

func (s *viewFaultStore) Put(ctx context.Context, key blob.Key, body []byte) error {
	if s.putSuffix != "" && strings.HasSuffix(string(key), s.putSuffix) {
		s.putSuffix = ""
		if s.putApplied {
			if err := s.Store.Put(ctx, key, body); err != nil {
				return err
			}
		}
		return errors.New("test put interrupted")
	}
	return s.Store.Put(ctx, key, body)
}

func (s *viewFaultStore) Delete(ctx context.Context, key blob.Key) error {
	if key == s.deleteKey {
		return errors.New("test delete interrupted")
	}
	return s.Store.Delete(ctx, key)
}

func setCompleteViewWiki(t *testing.T, f *fixture, pid, wiki string) {
	t.Helper()
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		project := fixtureProjectsGet(t, st, pid)
		project.Revision = wiki
		fixtureProjectsPut(t, st, pid, project)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// E9: ZIP PUT precedes the one SQL transaction for publication and child result.
func TestViewOverwriteRecoversFromInterruptedPublication(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"bundle", "SQL"} {
		t.Run(boundary, func(t *testing.T) {
			f, _, p := orgProject(t)
			ctx := t.Context()
			setCompleteViewWiki(t, f, p.ID, "wiki-1")
			v, err := f.s.viewBySlugLocked(ctx, p.ID, "project-map")
			fixtureCheck(t, err)
			fixtureViewBundle(t, f, p.ID, v.ID, "old-job", "wiki-1", []byte("old bundle"))
			child := fixtureViewJob(t, f, p.ID, v.ID, "new-job", "wiki-1")
			objects := f.s.Blobs
			restore := func() {}
			if boundary == "bundle" {
				f.s.Blobs = &viewFaultStore{Store: objects, putSuffix: ".zip", putApplied: true}
				f.s.storage.objects = f.s.Blobs
			} else {
				restore = fixtureSQLFault(t, f, "jobs", "UPDATE", "NEW.status='completed'")
			}
			bundle := &viewBundle{files: map[string][]byte{"index.html": []byte("new bundle")}}
			if err := f.s.storage.publishView(ctx, child, bundle); err == nil {
				t.Fatal("publication was not interrupted")
			}
			got := fixtureViewRow(t, f, p.ID, v.ID)
			if store.ValueString(got.Data.JobID) != "old-job" || storedJob(t, f, child).Status != "running" {
				t.Fatal("uncommitted publication visible", got)
			}
			body, err := readFixtureViewBundle(ctx, f.s, p.ID, v.Slug)
			fixtureCheck(t, err)
			if string(body) != "old bundle" {
				t.Fatal(string(body))
			}
			restore()
			f.s.Blobs = objects
			f.s.storage.objects = objects
			fixtureCheck(t, f.s.storage.publishView(ctx, child, bundle))
			got = fixtureViewRow(t, f, p.ID, v.ID)
			if store.ValueString(got.Data.JobID) != "new-job" || storedJob(t, f, child).Status != "completed" {
				t.Fatal("publication not atomic", got)
			}
		})
	}
}
func fixtureViewJob(t *testing.T, f *fixture, pid, vid, parent, snapshot string) string {
	t.Helper()
	child := parent + "-child"
	now := time.Now()
	fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
		p, err := q.Project(pid)
		if err != nil {
			return err
		}
		p.WikiRevision = &snapshot
		if err = q.PutProject(p); err != nil {
			return err
		}
		members, err := q.ProjectMembers(pid)
		if err != nil {
			return err
		}
		uid := ""
		for id, role := range members {
			if role == "owner" {
				uid = id
			}
		}
		if err = q.PutJob(store.JobRow{ID: parent, Kind: "generate", ProjectID: pid, Status: "running", GenerationSnapshot: &snapshot, Data: store.JobData{UserID: uid, ViewGeneration: &store.GenerationData{}, Views: []store.ViewResultData{{ViewID: vid, Slug: "project-map", Status: "running"}}}}); err != nil {
			return err
		}
		j := fixtureJob{ID: child, Kind: "view", ProjectID: pid, Status: "running", UserID: uid, ViewTask: &fixtureViewTask{ParentID: parent, ViewID: vid, Snapshot: snapshot}, Run: &fixtureAgentRun{Phase: model.PhaseFinalizing, PreparedAt: now, Deadline: now.Add(time.Hour)}}
		return q.PutJob(fixtureRow(j))
	}))
	return child
}
func TestViewEditDeleteAndSourceRemovalFenceResults(t *testing.T) {
	t.Parallel()
	f, token, p := orgProject(t)
	ctx := t.Context()
	setCompleteViewWiki(t, f, p.ID, "wiki-1")
	v, err := f.s.viewBySlugLocked(ctx, p.ID, "project-map")
	fixtureCheck(t, err)
	fixtureViewBundle(t, f, p.ID, v.ID, "old-job", "wiki-1", []byte("published"))
	child := fixtureViewJob(t, f, p.ID, v.ID, "late-job", "wiki-1")
	prompt := "New prompt"
	updated, err := f.s.updateViewLocked(ctx, p.ID, v.Slug, func() (viewSettings, error) {
		return parseViewSettingsJSON(requestBody{raw: []byte(`{"prompt":"New prompt"}`)})
	})
	fixtureCheck(t, err)
	if updated.Prompt != prompt || updated.JobID != "old-job" {
		t.Fatal(updated)
	}
	// A settings edit does not invalidate an already running job.
	fixtureCheck(t, f.s.storage.publishView(ctx, child, &viewBundle{files: map[string][]byte{"index.html": []byte("late")}}))
	actor := storedJob(t, f, child).UserID
	fixtureCheck(t, f.stateStore.Write(ctx, func(q *store.Queries) error {
		j := store.JobRow{ID: "remove-source", ProjectID: p.ID, Status: "running", Kind: "generate", Phase: store.NullableString("finalizing"), Data: store.JobData{UserID: actor, Run: &store.RunData{BudgetCents: 100}}}
		if err := q.PutJob(j); err != nil {
			return err
		}
		return q.PutInbox(store.InboxRow{ID: "remove", ProjectID: p.ID, SourceURL: "https://example.com/source", Remove: true, RunID: &j.ID})
	}))
	_, err = f.s.storage.publishWiki(ctx, "remove-source", map[string]string{"wiki/index.md": "updated"})
	fixtureCheck(t, err)
	if _, err := readFixtureViewBundle(ctx, f.s, p.ID, v.Slug); err == nil {
		t.Fatal("obsolete bundle visible")
	}
	if code := f.request("DELETE", "/api/projects/"+p.ID+"/views/"+v.Slug, token, nil, nil); code != 204 {
		t.Fatal(code)
	}
	if _, err := f.s.viewBySlugLocked(ctx, p.ID, v.Slug); err == nil {
		t.Fatal("deleted default view returned")
	}
	replacement, err := f.s.createViewLocked(ctx, p.ID, requestBody{raw: []byte(`{"slug":"` + v.Slug + `","title":"Replacement","prompt":"New prompt"}`)})
	fixtureCheck(t, err)
	if replacement.ID == v.ID {
		t.Fatal("reused ID")
	}
	_, err = f.s.storage.collectGarbage(ctx, p.ID)
	fixtureCheck(t, err)
	keys, err := f.s.Blobs.List(ctx, "projects/"+p.ID+"/.kioku/views/"+v.ID+"/")
	fixtureCheck(t, err)
	if len(keys) != 0 {
		t.Fatal(keys)
	}
}
func TestViewEditSQLFailurePreservesSettings(t *testing.T) {
	t.Parallel()
	f, token, p := orgProject(t)
	api := "/api/projects/" + p.ID + "/views/project-map"
	var before, after viewDefinition
	if c := f.request("GET", api, token, nil, &before); c != 200 {
		t.Fatal(c)
	}
	restore := fixtureSQLFault(t, f, "views", "UPDATE", "1")
	if c := f.request("PATCH", api, token, map[string]string{"title": "Replacement"}, nil); c != 500 {
		t.Fatal(c)
	}
	restore()
	if c := f.request("GET", api, token, nil, &after); c != 200 || before.Title != after.Title || before.Prompt != after.Prompt {
		t.Fatal(c, before, after)
	}
}
func TestWikiViewPatchUsesSQLWithoutObjectReads(t *testing.T) {
	t.Parallel()
	f, token, p := orgProject(t)
	api := "/api/projects/" + p.ID + "/views/wiki"
	var v viewDefinition
	if c := f.request("PATCH", api, token, map[string]string{"title": "Saved", "prompt": "Saved prompt"}, &v); c != 200 {
		t.Fatal(c)
	}
	f.s.Blobs = unavailableWikiRead{f.s.Blobs}
	f.s.storage.objects = f.s.Blobs
	if c := f.request("PATCH", api, token, map[string]string{"title": "New"}, &v); c != 200 || v.Title != "New" || v.Prompt != "Saved prompt" {
		t.Fatal(c, v)
	}
	restore := fixtureSQLFault(t, f, "views", "UPDATE", "1")
	if c := f.request("PATCH", api, token, map[string]string{"prompt": "Lost"}, nil); c != 500 {
		t.Fatal(c)
	}
	restore()
	if c := f.request("GET", api, token, nil, &v); c != 200 || v.Prompt != "Saved prompt" {
		t.Fatal(c, v)
	}
}

func readFixtureViewBundle(ctx context.Context, s *Server, pid, slug string) ([]byte, error) {
	unlock, err := s.storage.lock(ctx, pid)
	if err != nil {
		return nil, err
	}
	defer unlock()
	v, err := s.viewBySlugLocked(ctx, pid, slug)
	if err != nil {
		return nil, err
	}
	return s.readViewBundleLocked(ctx, pid, v)
}
