package server

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

type viewFaultStore struct {
	blob.Store
	putSuffix  string
	putApplied bool
	deleteKey  blob.Key
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

func setCompleteViewWiki(t *testing.T, f *fixture, pid, wiki, epoch string) {
	t.Helper()
	if err := f.stateStore.Update(func(st *model.State) error {
		project := st.Projects[pid]
		project.WikiCompleteRevision, project.ViewEpoch = wiki, epoch
		st.Projects[pid] = project
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestViewOverwriteRecoversFromInterruptedPublication(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		applied      bool
		wantJob      string
		wantBundle   string
	}{
		{"bundle", "/bundle.zip", true, "old-job", "old bundle"},
		{"metadata", "/view.json", true, "old-job", "old bundle"},
		{"before commit", "/index.json", false, "old-job", "old bundle"},
		{"after commit", "/index.json", true, "new-job", "new bundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, p := orgProject(t)
			ctx := t.Context()
			setCompleteViewWiki(t, f, p.ID, "wiki-1", "epoch-1")
			unlock, err := f.s.lockProject(ctx, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()

			view, err := f.s.viewBySlugLocked(ctx, p.ID, "project-map")
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := f.s.publishViewLocked(ctx, p.ID, view.ID, view.Version, "old-job", "wiki-1", []byte("old bundle")); !ok || err != nil {
				t.Fatalf("initial publication = %v, %v", ok, err)
			}
			f.s.Blobs = &viewFaultStore{Store: f.s.Blobs, putSuffix: tc.suffix, putApplied: tc.applied}
			if _, err := f.s.publishViewLocked(ctx, p.ID, view.ID, view.Version, "new-job", "wiki-1", []byte("new bundle")); err == nil {
				t.Fatal("publication was not interrupted")
			}

			view, err = f.s.viewByIDLocked(ctx, p.ID, view.ID)
			if err != nil || view.JobID != tc.wantJob || view.GeneratedAt == nil || view.WikiSnapshotID != "wiki-1" {
				t.Fatalf("recovered metadata = %+v, %v", view, err)
			}
			body, err := f.s.readViewBundleLocked(ctx, p.ID, view)
			if err != nil || string(body) != tc.wantBundle {
				t.Fatalf("recovered bundle = %q, %v", body, err)
			}
			for _, name := range []string{"publish.json", "previous.zip", "candidate.zip"} {
				if _, err := f.s.Blobs.Get(ctx, viewKey(p.ID, view.ID, name)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("temporary object %s is still present: %v", name, err)
				}
			}
		})
	}
}

func TestViewEditDeleteAndSourceRemovalFenceResults(t *testing.T) {
	f, _, p := orgProject(t)
	ctx := t.Context()
	setCompleteViewWiki(t, f, p.ID, "wiki-1", "epoch-1")
	unlock, err := f.s.lockProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	view, err := f.s.viewBySlugLocked(ctx, p.ID, "project-map")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.s.publishViewLocked(ctx, p.ID, view.ID, view.Version, "job-1", "wiki-1", []byte("published")); !ok || err != nil {
		t.Fatalf("publication = %v, %v", ok, err)
	}
	if ok, err := f.s.setViewStatusLocked(ctx, p.ID, view.ID, view.Version, "running", ""); !ok || err != nil {
		t.Fatalf("set running = %v, %v", ok, err)
	}
	prompt := "New prompt"
	updated, err := f.s.updateViewLocked(ctx, p.ID, view.Slug, nil, &prompt)
	if err != nil || updated.Prompt != prompt || updated.Status != "skipped" || updated.JobID != "job-1" {
		t.Fatalf("updated view = %+v, %v", updated, err)
	}
	if ok, err := f.s.publishViewLocked(ctx, p.ID, view.ID, view.Version, "late-job", "wiki-1", []byte("late")); ok || err != nil {
		t.Fatalf("outdated publication = %v, %v", ok, err)
	}

	setCompleteViewWiki(t, f, p.ID, "wiki-2", "epoch-2")
	if _, err := f.s.readViewBundleLocked(ctx, p.ID, updated); err == nil {
		t.Fatal("obsolete bundle was still visible before cleanup")
	}
	faulty := &viewFaultStore{Store: f.s.Blobs, deleteKey: viewKey(p.ID, view.ID, "bundle.zip")}
	f.s.Blobs = faulty
	if err := f.s.invalidateViewsLocked(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	invalidated, err := f.s.viewBySlugLocked(ctx, p.ID, view.Slug)
	if err != nil || invalidated.GeneratedAt != nil || invalidated.JobID != "" || invalidated.WikiSnapshotID != "" {
		t.Fatalf("invalidated view metadata = %+v, %v", invalidated, err)
	}
	if _, err := faulty.Store.Get(ctx, faulty.deleteKey); err != nil {
		t.Fatalf("fixture should still have the obsolete physical object: %v", err)
	}

	if err := f.s.deleteViewLocked(ctx, p.ID, view.Slug); err != nil {
		t.Fatal(err)
	}
	if err := f.s.seedViewsLocked(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.viewBySlugLocked(ctx, p.ID, view.Slug); err == nil {
		t.Fatal("default view reappeared after deletion")
	}
	replacement, err := f.s.createViewLocked(ctx, p.ID, view.Slug, "Replacement", prompt)
	if err != nil || replacement.ID == view.ID {
		t.Fatalf("replacement view = %+v, %v", replacement, err)
	}
	if ok, err := f.s.publishViewLocked(ctx, p.ID, view.ID, updated.Version, "deleted-view-job", "wiki-2", []byte("late")); ok || err != nil {
		t.Fatalf("deleted view publication = %v, %v", ok, err)
	}
	faulty.deleteKey = ""
	if err := f.s.collectViewsLocked(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Blobs.Get(ctx, viewKey(p.ID, view.ID, "bundle.zip")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("orphaned bundle not removed: %v", err)
	}
}

func TestViewEditRecoversAcrossSettingsCommit(t *testing.T) {
	for _, tc := range []struct {
		suffix  string
		applied bool
		commits bool
	}{
		{"/index.json", false, false},
		{"/index.json", true, true},
		{"/view.json", false, true},
	} {
		f, _, p := orgProject(t)
		ctx := t.Context()
		setCompleteViewWiki(t, f, p.ID, "wiki-1", "epoch-1")
		unlock, err := f.s.lockProject(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		prior, err := f.s.viewBySlugLocked(ctx, p.ID, "project-map")
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := f.s.setViewStatusLocked(ctx, p.ID, prior.ID, prior.Version, "running", ""); !ok || err != nil {
			t.Fatalf("set running = %v, %v", ok, err)
		}
		f.s.Blobs = &viewFaultStore{Store: f.s.Blobs, putSuffix: tc.suffix, putApplied: tc.applied}
		prompt := "Updated prompt"
		if _, err := f.s.updateViewLocked(ctx, p.ID, prior.Slug, nil, &prompt); err == nil {
			t.Fatal("settings update was not interrupted")
		}
		view, err := f.s.viewBySlugLocked(ctx, p.ID, prior.Slug)
		if err != nil {
			t.Fatal(err)
		}
		if tc.commits {
			if view.Prompt != prompt || view.Status != "skipped" || view.Version == prior.Version {
				t.Errorf("committed settings were not recovered: %+v", view)
			}
		} else if view.Prompt != prior.Prompt || view.Status != "running" || view.Version != prior.Version {
			t.Errorf("uncommitted settings leaked: %+v", view)
		}
		if _, err := f.s.Blobs.Get(ctx, viewKey(p.ID, prior.ID, "edit.json")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("temporary settings record remains: %v", err)
		}
	}
}
