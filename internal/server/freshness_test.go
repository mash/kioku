package server

import (
	"context"
	"errors"
	"testing"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

// An unchanged generate must not need source downloads or new object writes.
type noGenerationObjects struct{ blob.Store }

func (noGenerationObjects) Get(context.Context, blob.Key) ([]byte, error) {
	return nil, errors.New("unchanged generation read an object")
}
func (noGenerationObjects) Put(context.Context, blob.Key, []byte) error {
	return errors.New("unchanged generation wrote an object")
}

func TestManualGenerateSkipsUnchangedSourcesAndPreservesTimestamps(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	receipt := putFile(t, f, token, p.ID, "note.md", "Evidence")
	generate := func() model.Job {
		t.Helper()
		job := startManaged(t, f, token, p.ID, false)
		if job.Run != nil {
			sendOutput(t, f, ma, job, "Fresh wiki")
			ma.SetStatus(job.Run.Ref.ID, "idle", "end_turn", 123)
			job = reconcileManaged(t, f, job)
		}
		return job
	}
	first := generate()
	initialCalls := len(ma.Requests())
	var before model.Project
	var srcBefore model.Source
	_ = f.db.View(func(st model.State) error {
		before = st.Projects[p.ID]
		srcBefore = st.Sources[receipt.SourceIDs[0]]
		if before.SourcesUpdatedAt.IsZero() || before.WikiUpdatedAt.IsZero() || !srcBefore.UpdatedAt.Equal(before.SourcesUpdatedAt) {
			t.Error("missing source/wiki timestamps", before, srcBefore)
		}
		rev := st.Revisions[first.Revision]
		if !rev.SourcesUpdatedAt.Equal(before.SourcesUpdatedAt) {
			t.Error("revision did not capture source timestamp", rev)
		}
		return nil
	})
	putFile(t, f, token, p.ID, "note.md", "Evidence")
	objects := f.s.Blobs
	f.s.Blobs = noGenerationObjects{objects}
	// Changing provider settings is not a source change and must not force a
	// regeneration.
	f.runnerConfig.MAAgentID = "different-model"
	f.setRunners()
	for range 2 {
		job := generate()
		if !job.Unchanged || job.Status != "completed" || job.Revision != first.Revision {
			t.Fatal("unchanged generate did not retain revision", job)
		}
	}
	_ = f.db.View(func(st model.State) error {
		p := st.Projects[p.ID]
		src := st.Sources[receipt.SourceIDs[0]]
		if p.SourceVersion != before.SourceVersion || !p.SourcesUpdatedAt.Equal(before.SourcesUpdatedAt) || !src.UpdatedAt.Equal(srcBefore.UpdatedAt) {
			t.Error("identical put changed source version or timestamps", p, src)
		}
		if p.Revision != before.Revision || !p.WikiUpdatedAt.Equal(before.WikiUpdatedAt) || len(st.Revisions) != 1 {
			t.Error("unchanged generation updated wiki", p)
		}
		return nil
	})
	if len(ma.Requests()) != initialCalls {
		t.Fatal("unchanged generate called model", len(ma.Requests()))
	}
	f.s.Blobs = objects
	f.runnerConfig.MAAgentID = "test-model"
	f.setRunners()
	putFile(t, f, token, p.ID, "new.md", "Changed inputs")
	changed := generate()
	if changed.Unchanged || changed.Revision == first.Revision || len(ma.Requests()) <= initialCalls {
		t.Fatal("changed inputs were not generated", changed, len(ma.Requests()))
	}
	_ = f.db.View(func(st model.State) error {
		p := st.Projects[p.ID]
		if !p.SourcesUpdatedAt.After(before.SourcesUpdatedAt) || !p.WikiUpdatedAt.After(before.WikiUpdatedAt) || p.SourceVersion != p.WikiSourceVersion {
			t.Error("change timestamps or coverage not advanced", p)
		}
		return nil
	})
}

func TestEmptyProjectGenerateCompletesWithoutModelOrRevision(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	job := startManaged(t, f, token, p.ID, false)
	if !job.Unchanged || job.Status != "completed" || job.Revision != "" || len(ma.Requests()) != 0 {
		t.Fatal(job)
	}
	_ = f.db.View(func(st model.State) error {
		if len(st.Revisions) != 0 || !st.Projects[p.ID].WikiUpdatedAt.IsZero() {
			t.Error("empty wiki updated")
		}
		return nil
	})
}
