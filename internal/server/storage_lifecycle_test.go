package server

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
)

func storageRunning(t *testing.T, o *storageOperations, id string) store.JobRow {
	t.Helper()
	j := store.JobRow{ID: id, ProjectID: "p", Status: "running", Kind: "generate", InputReady: true, CleanupComplete: boolPointer(false), Phase: nullableString("output_received"), Data: store.JobData{UserID: "u", Run: &store.RunData{PreparedAt: 1000, Deadline: 3601000, BudgetCents: 1000}}}
	sqlWrite(t, o.db, func(q *store.Queries) error { return q.PutJob(j) })
	return j
}
func boolPointer(v bool) *bool { return &v }
func readSQLJob(t *testing.T, o *storageOperations, id string) store.JobRow {
	t.Helper()
	var j store.JobRow
	if err := o.db.Read(t.Context(), func(q *store.Queries) error { var err error; j, err = q.Job(id); return err }); err != nil {
		t.Fatal(err)
	}
	return j
}
func TestPartialWikiKeepsCompletedCapacity(t *testing.T) {
	o, b := storageFixture(t)
	storageRunning(t, o, "j")
	old := []byte("old content")
	if err := b.Put(t.Context(), model.WikiContentKey("p", model.HashContent(old)), old); err != nil {
		t.Fatal(err)
	}
	sqlWrite(t, o.db, func(q *store.Queries) error {
		p, e := q.Project("p")
		if e != nil {
			return e
		}
		p.WikiRevision = nullableString("old")
		if e = q.PutProject(p); e != nil {
			return e
		}
		return q.PutWikiFile(store.WikiFileRow{ProjectID: "p", Path: "wiki/index.md", Hash: string(model.HashContent(old)), Data: store.WikiFileData{Bytes: int64(len(old))}})
	})
	puts := 0
	b.put = func(ctx context.Context, k blob.Key, v []byte) error {
		puts++
		if puts == 2 {
			return errors.New("PUT interrupted")
		}
		return o.db.Write(ctx, func(q *store.Queries) error { return nil })
	}
	_, err := o.publishWiki(t.Context(), "j", map[string]string{"wiki/a.md": "A", "wiki/index.md": "new"})
	if err == nil {
		t.Fatal("expected interruption")
	}
	if err = o.db.Read(t.Context(), func(q *store.Queries) error {
		p, e := q.Project("p")
		if !p.WikiIncomplete || p.Data.WikiCompleteBytes == nil || *p.Data.WikiCompleteBytes != int64(len(old)) || valueString(p.Data.WikiCompleteRevision) != "old" {
			t.Fatal(p)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	got, err := readFixtureWikiFile(t.Context(), o, "wiki/index.md")
	if err != nil || got != string(old) {
		t.Fatal(got, err)
	}
	b.put = nil
	rev, err := o.publishWiki(t.Context(), "j", map[string]string{"wiki/index.md": "new"})
	if err != nil {
		t.Fatal(err)
	}
	if err = o.db.Read(t.Context(), func(q *store.Queries) error {
		p, e := q.Project("p")
		if e != nil {
			return e
		}
		files, e := q.ProjectWiki("p")
		if p.WikiIncomplete || p.Data.WikiCompleteBytes != nil || valueString(p.Data.WikiCompleteRevision) != rev || len(files) != 1 {
			t.Fatal(p, files)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}
func TestViewAndChildCompletionCommitTogether(t *testing.T) {
	o, b := storageFixture(t)
	child := storageRunning(t, o, "child")
	sqlWrite(t, o.db, func(q *store.Queries) error {
		p, e := q.Project("p")
		if e != nil {
			return e
		}
		p.WikiRevision = nullableString("wiki1")
		if e = q.PutProject(p); e != nil {
			return e
		}
		if e = q.PutView(store.ViewRow{ID: "v", ProjectID: "p", Slug: "map", Data: sqlTestViewData()}); e != nil {
			return e
		}
		if e = q.PutJob(store.JobRow{ID: "parent", ProjectID: "p", Status: "running", Kind: "generate", ChildID: nullableString("child"), GenerationSnapshot: nullableString("wiki1"), Data: store.JobData{UserID: "u", ViewGeneration: &store.GenerationData{}, Views: []store.ViewResultData{{ViewID: "v"}}}}); e != nil {
			return e
		}
		child.Kind = "view"
		child.ParentID = nullableString("parent")
		child.Data.ViewTask = &store.ViewTaskData{ViewID: "v", Snapshot: "wiki1"}
		return q.PutJob(child)
	})
	bundle := &viewBundle{files: map[string][]byte{"index.html": []byte("<h1>ok</h1>")}}
	b.put = func(ctx context.Context, k blob.Key, body []byte) error {
		return o.db.Write(ctx, func(q *store.Queries) error {
			v, e := q.View("p", "v")
			if e != nil {
				return e
			}
			j, e := q.Job("child")
			if v.BundleHash != nil || j.Status != "running" {
				t.Fatal(v, j)
			}
			return e
		})
	}
	if err := o.publishView(t.Context(), "child", bundle); err != nil {
		t.Fatal(err)
	}
	if err := o.db.Read(t.Context(), func(q *store.Queries) error {
		v, e := q.View("p", "v")
		if e != nil {
			return e
		}
		j, e := q.Job("child")
		if v.BundleHash == nil || valueString(v.Data.JobID) != "parent" || j.Status != "completed" {
			t.Fatal(v, j)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}
func TestCleanupUnreferencesBeforeDelete(t *testing.T) {
	o, b := storageFixture(t)
	j := storageRunning(t, o, "j")
	j.Status = "completed"
	sqlWrite(t, o.db, func(q *store.Queries) error { return q.PutJob(j) })
	if err := b.Put(t.Context(), sqlJobKey(j, "input.tar.gz"), []byte("in")); err != nil {
		t.Fatal(err)
	}
	b.del = func(ctx context.Context, k blob.Key) error {
		return o.db.Write(ctx, func(q *store.Queries) error {
			j, e := q.Job("j")
			if e != nil {
				return e
			}
			_, e = q.Usage("j")
			if j.InputReady || valueBool(j.CleanupComplete) {
				t.Fatal(j)
			}
			return e
		})
	}
	cleaned := 0
	cost := int64(40)
	cleanup := func(ctx context.Context, a runner.Attempt) error {
		cleaned++
		return o.db.Write(ctx, func(q *store.Queries) error {
			j, e := q.Job("j")
			if e != nil {
				return e
			}
			u, e := q.Usage("j")
			if !j.InputReady || u.Data.CostCents == nil || *u.Data.CostCents != 40 {
				t.Fatal(j, u)
			}
			return e
		})
	}
	if done, err := o.cleanup(t.Context(), "j", runner.Status{State: runner.StateStopped, Usage: runner.UsageSample{CostCents: &cost}}, cleanup); err != nil || !done {
		t.Fatal(done, err)
	}
	if cleaned != 1 || !valueBool(readSQLJob(t, o, "j").CleanupComplete) {
		t.Fatal(cleaned)
	}
}
func TestParentConsumesChildCostOnce(t *testing.T) {
	for _, cost := range []*int64{nil, new(int64), func() *int64 { v := int64(40); return &v }()} {
		o, _ := storageFixture(t)
		child := storageRunning(t, o, "child")
		child.Status = "completed"
		child.InputReady = false
		child.CleanupComplete = boolPointer(true)
		child.ParentID = nullableString("parent")
		child.Data.ViewTask = &store.ViewTaskData{ViewID: "v1", Snapshot: "w"}
		sqlWrite(t, o.db, func(q *store.Queries) error {
			p, e := q.Project("p")
			if e != nil {
				return e
			}
			p.WikiRevision = nullableString("w")
			if e = q.PutProject(p); e != nil {
				return e
			}
			for _, v := range []string{"v1", "v2"} {
				if e = q.PutView(store.ViewRow{ID: v, ProjectID: "p", Slug: v, Data: sqlTestViewData()}); e != nil {
					return e
				}
			}
			if e = q.PutJob(child); e != nil {
				return e
			}
			if e = q.RecordUsage(store.UsageRow{AttemptID: "child", JobID: "child", ProjectID: "p", Data: model.UsageValues{CostCents: cost}}); e != nil {
				return e
			}
			return q.PutJob(store.JobRow{ID: "parent", ProjectID: "p", Status: "running", Kind: "generate", ChildID: nullableString("child"), GenerationSnapshot: nullableString("w"), Data: store.JobData{UserID: "u", Views: []store.ViewResultData{{ViewID: "v1"}, {ViewID: "v2"}}, ViewGeneration: &store.GenerationData{Remaining: 100}}})
		})
		if err := o.advanceViews(t.Context(), "parent"); err != nil {
			t.Fatal(err)
		}
		p := readSQLJob(t, o, "parent")
		want := int64(100)
		if cost != nil {
			want -= *cost
		}
		if p.Data.ViewGeneration.Remaining != want || p.ChildID == nil || *p.ChildID == "child" {
			t.Fatal(p)
		}
		if err := o.advanceViews(t.Context(), "parent"); err != nil {
			t.Fatal(err)
		}
		if p2 := readSQLJob(t, o, "parent"); p2.Data.ViewGeneration.Remaining != want || *p2.ChildID != *p.ChildID {
			t.Fatal(p2)
		}
	}
}
func TestFileOutputWaitSurvivesRestart(t *testing.T) {
	o, _ := storageFixture(t)
	j := storageRunning(t, o, "j")
	j.Data.Run.UsesProviderFiles = true
	j.Phase = nullableString("agent_running")
	sqlWrite(t, o.db, func(q *store.Queries) error { return q.PutJob(j) })
	if _, expired, e := o.beginFileWait(t.Context(), "j"); e != nil || expired {
		t.Fatal(expired, e)
	}
	resumed := newStorageOperations(o.db, o.objects, o.config)
	resumed.now = func() time.Time { return time.UnixMilli(91000) }
	j, expired, e := resumed.beginFileWait(t.Context(), "j")
	if e != nil || !expired || *j.Data.Run.OutputWaitSince != 1000 {
		t.Fatal(j, expired, e)
	}
}
func TestHTTPOutputWinsBeforeNoOutputFailure(t *testing.T) {
	o, _ := storageFixture(t)
	storageRunning(t, o, "j")
	wins, e := o.failJob(t.Context(), "j", "stopped without output", &runner.Status{State: runner.StateStopped}, true)
	if e != nil || !wins || readSQLJob(t, o, "j").Status != "running" {
		t.Fatal(wins, e)
	}
}

func sqlTestViewData() store.ViewData {
	return store.ViewData{Title: nullableString("Map"), Prompt: nullableString("Test"), Status: nullableString("pending")}
}

func TestCleanupRestartNeverChargesTwice(t *testing.T) {
	o, b := storageFixture(t)
	j := storageRunning(t, o, "j")
	j.GenerationSnapshot = nullableString("w")
	j.Data.ViewGeneration = &store.GenerationData{Remaining: 100}
	sqlWrite(t, o.db, func(q *store.Queries) error { return q.PutJob(j) })
	if e := b.Put(t.Context(), sqlJobKey(j, "input.tar.gz"), []byte("in")); e != nil {
		t.Fatal(e)
	}
	cost := int64(40)
	status := runner.Status{State: runner.StateStopped, Usage: runner.UsageSample{CostCents: &cost}}
	b.del = func(context.Context, blob.Key) error { return errors.New("delete unavailable") }
	if done, e := o.cleanup(t.Context(), "j", status, nil); e == nil || done {
		t.Fatal(done, e)
	}
	if got := readSQLJob(t, o, "j"); got.InputReady || valueBool(got.CleanupComplete) || got.Data.ViewGeneration.Remaining != 100 {
		t.Fatal(got)
	}
	// A new instance sees the stored usage and ignores a later known value.
	resumed := newStorageOperations(o.db, b, o.config)
	resumed.now = o.now
	b.del = nil
	late := int64(88)
	status.Usage.CostCents = &late
	if done, e := resumed.cleanup(t.Context(), "j", status, nil); e != nil || !done {
		t.Fatal(done, e)
	}
	if done, e := resumed.cleanup(t.Context(), "j", status, nil); e != nil || !done {
		t.Fatal(done, e)
	}
	if got := readSQLJob(t, o, "j"); got.Data.ViewGeneration.Remaining != 60 {
		t.Fatal(got)
	}
	if e := o.db.Read(t.Context(), func(q *store.Queries) error {
		u, e := q.Usage("j")
		if u.Data.CostCents == nil || *u.Data.CostCents != 40 {
			t.Fatal(u)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}

func TestCleanupResumesAfterLastProviderSessionWasDeleted(t *testing.T) {
	for _, cost := range []*int64{nil, sqlTestPtr(int64(40))} {
		o, b := storageFixture(t)
		j := storageRunning(t, o, "j")
		j.Status = "completed"
		j.SessionID = nullableString("deleted-session")
		j.Data.Run.UsesProviderFiles = true
		j.Data.Run.UploadStarted = true
		sqlWrite(t, o.db, func(q *store.Queries) error { return q.PutJob(j) })
		if err := b.Put(t.Context(), sqlJobKey(j, "input.tar.gz"), []byte("in")); err != nil {
			t.Fatal(err)
		}
		status := runner.Status{State: runner.StateStopped, Usage: runner.UsageSample{CostCents: cost}}
		if done, err := o.cleanup(t.Context(), "j", status, func(context.Context, runner.Attempt) error {
			// The last session DELETE succeeded, but deleting its input file failed.
			return errors.New("provider input deletion failed")
		}); err == nil || done {
			t.Fatal(done, err)
		}
		resumed := newStorageOperations(o.db, b, o.config)
		resumed.now = func() time.Time { return time.UnixMilli(2000) }
		cleaned := 0
		cleanup := func(context.Context, runner.Attempt) error { cleaned++; return nil }
		for _, state := range []runner.State{runner.StateUnknown, runner.StateRunning} {
			if done, err := resumed.cleanup(t.Context(), "j", runner.Status{State: state}, cleanup); err != nil || done || cleaned != 0 {
				t.Fatalf("cleanup ignored a newly discovered session: %v %v %d", done, err, cleaned)
			}
		}
		// A fresh inspection now finds no sessions. Recorded usage proves that
		// this is resumed cleanup, rather than a not-yet-visible Start.
		if done, err := resumed.cleanup(t.Context(), "j", runner.Status{State: runner.StateNone}, cleanup); err != nil || !done || cleaned != 1 {
			t.Fatalf("cleanup delayed after session deletion: %v %v %d", done, err, cleaned)
		}
		if got := readSQLJob(t, resumed, "j"); got.InputReady || !valueBool(got.CleanupComplete) {
			t.Fatal(got)
		}
		if err := o.db.Read(t.Context(), func(q *store.Queries) error {
			u, err := q.Usage("j")
			if (u.Data.CostCents == nil) != (cost == nil) || cost != nil && *u.Data.CostCents != *cost {
				t.Fatalf("cleanup changed the first usage: %#v", u.Data)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCleanupLockStateDoesNotGrowWithJobHistory(t *testing.T) {
	o, _ := storageFixture(t)
	locks := 0
	for i := range 32 {
		id := strconv.Itoa(i)
		j := storageRunning(t, o, id)
		j.Status = "completed"
		sqlWrite(t, o.db, func(q *store.Queries) error { return q.PutJob(j) })
		unlock, err := o.lock(t.Context(), "generate:p")
		fixtureCheck(t, err)
		done, err := o.cleanup(t.Context(), id, runner.Status{State: runner.StateStopped}, nil)
		unlock()
		if err != nil || !done {
			t.Fatal(done, err)
		}
		count := 0
		o.gates.Range(func(_, _ any) bool { count++; return true })
		if i == 0 {
			locks = count
		} else if count != locks {
			t.Fatalf("completed jobs retain lock state: first=%d now=%d", locks, count)
		}
	}
}

func TestOutputPutBeforeReferenceCommitAndBoundedReplacement(t *testing.T) {
	o, b := storageFixture(t)
	j := storageRunning(t, o, "j")
	j.Phase = nullableString("agent_running")
	sqlWrite(t, o.db, func(q *store.Queries) error { return q.PutJob(j) })
	first, e := o.saveOutput(t.Context(), "j", []byte("one"), nil)
	if e != nil {
		t.Fatal(e)
	}
	b.put = func(ctx context.Context, k blob.Key, v []byte) error {
		return o.db.Write(ctx, func(q *store.Queries) error {
			j, e := q.Job("j")
			if valueString(j.OutputHash) != first {
				t.Fatal("early reference", j)
			}
			return e
		})
	}
	// A rejected post-PUT authorization leaves the previously selected output.
	checks := 0
	_, e = o.saveOutput(t.Context(), "j", []byte("two"), func(*store.Queries) error {
		checks++
		if checks == 2 {
			return fail(403, "revoked")
		}
		return nil
	})
	if e == nil {
		t.Fatal("post PUT check ignored")
	}
	b.put = nil
	b.del = func(context.Context, blob.Key) error { return errors.New("delete failed") }
	_, e = o.saveOutput(t.Context(), "j", []byte("three"), nil)
	var api apiError
	if !errors.As(e, &api) || api.status != 503 {
		t.Fatal(e)
	}
	keys, e := b.List(t.Context(), "projects/p/.kioku/jobs/j/")
	if e != nil || len(keys) != 2 {
		t.Fatal(keys, e)
	}
	b.del = nil
	third, e := o.saveOutput(t.Context(), "j", []byte("three"), nil)
	if e != nil {
		t.Fatal(e)
	}
	if got := readSQLJob(t, o, "j"); valueString(got.OutputHash) != third {
		t.Fatal(got)
	}
}
func TestFailedInputNeverCreatesRunOrUsage(t *testing.T) {
	o, b := storageFixture(t)
	sqlWrite(t, o.db, func(q *store.Queries) error {
		if e := q.PutJob(store.JobRow{ID: "j", ProjectID: "p", Kind: "generate", Status: "queued", Data: store.JobData{UserID: "u"}}); e != nil {
			return e
		}
		return q.PutInbox(store.InboxRow{ID: "i", ProjectID: "p", SourceURL: "s"})
	})
	b.put = func(context.Context, blob.Key, []byte) error { return errors.New("upload failed") }
	if _, e := o.prepareInput(t.Context(), "j", false, "", func(context.Context, storageInput) ([]byte, error) { return []byte("in"), nil }); e == nil {
		t.Fatal("missing failure")
	}
	j := readSQLJob(t, o, "j")
	if j.Status != "failed" || j.Data.Run != nil {
		t.Fatal(j)
	}
	if e := o.db.Read(t.Context(), func(q *store.Queries) error {
		if _, e := q.Usage("j"); !errors.Is(e, sql.ErrNoRows) {
			t.Fatal(e)
		}
		i, e := q.Inbox("i")
		if i.RunID != nil {
			t.Fatal(i)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}
func TestAbortDeadlineWaitsStrictlyPastTenMinutes(t *testing.T) {
	o, _ := storageFixture(t)
	j := storageRunning(t, o, "j")
	o.now = func() time.Time { return time.UnixMilli(j.Data.Run.Deadline + 600000) }
	if _, e := o.failJob(t.Context(), "j", "deadline", nil, false); e != nil {
		t.Fatal(e)
	}
	if got := readSQLJob(t, o, "j"); got.Status != "running" || valueString(got.Data.Run.AbortReason) != "deadline" {
		t.Fatal(got)
	}
	o.now = func() time.Time { return time.UnixMilli(j.Data.Run.Deadline + 600001) }
	if _, e := o.failJob(t.Context(), "j", "later", nil, false); e != nil {
		t.Fatal(e)
	}
	if got := readSQLJob(t, o, "j"); got.Status != "failed" || valueString(got.Data.Error) != "deadline" {
		t.Fatal(got)
	}
	if done, e := o.cleanup(t.Context(), "j", runner.Status{State: runner.StateRunning}, nil); e != nil || done {
		t.Fatal(done, e)
	}
}
