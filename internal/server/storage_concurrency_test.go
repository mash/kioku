package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
)

func TestConcurrentPutReadPublishAndGC(t *testing.T) {
	t.Parallel()
	o, _ := storageFixture(t)
	body := []byte("initial")
	if err := o.objects.Put(t.Context(), model.WikiContentKey("p", model.HashContent(body)), body); err != nil {
		t.Fatal(err)
	}
	sqlWrite(t, o.db, func(q *store.Queries) error {
		return q.PutWikiFile(store.WikiFileRow{ProjectID: "p", Path: "wiki/index.md", Hash: string(model.HashContent(body)), Data: store.WikiFileData{Bytes: int64(len(body))}})
	})
	work := []func(int) error{
		func(i int) error {
			return o.putInbox(t.Context(), store.InboxRow{ID: fmt.Sprintf("i-%d", i), ProjectID: "p", SourceURL: "https://example.com/source", Data: store.InboxData{Submitter: "u"}}, []byte(fmt.Sprintf("source %d", i)))
		},
		func(i int) error { _, err := readFixtureWikiFile(t.Context(), o, "wiki/index.md"); return err },
		func(i int) error {
			id := fmt.Sprintf("j-%d", i)
			// Upload the input and commit the job under the content gate, as
			// prepareInput does.
			unlock, err := o.lock(t.Context(), "p")
			if err != nil {
				return err
			}
			j := store.JobRow{ID: id, ProjectID: "p", Status: "running", Kind: "generate", Phase: store.NullableString("output_received"), CleanupComplete: false, Data: store.JobData{UserID: "u", Run: &store.RunData{PreparedAt: 1000, Deadline: 3601000, BudgetCents: 100}}}
			err = o.objects.Put(t.Context(), sqlJobKey(j, "input.tar.gz"), []byte("input"))
			if err == nil {
				err = o.db.Write(t.Context(), func(q *store.Queries) error { return q.PutJob(j) })
			}
			unlock()
			if err != nil {
				return err
			}
			_, err = o.publishWiki(t.Context(), id, map[string]string{"wiki/index.md": fmt.Sprintf("wiki %d", i)})
			return err
		},
		func(i int) error { _, err := o.collectGarbage(t.Context(), "p"); return err },
	}
	ready := make(chan struct{})
	done := make(chan error, len(work))
	for _, fn := range work {
		go func() {
			<-ready
			for i := 0; i < 20; i++ {
				if err := fn(i); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	close(ready)
	for range work {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	assertStoredReferences(t, o)
	got, err := readFixtureWikiFile(t.Context(), o, "wiki/index.md")
	if err != nil || got != "wiki 19" {
		t.Fatal(got, err)
	}
	if err := o.db.Read(t.Context(), func(q *store.Queries) error {
		items, err := q.ProjectInbox("p")
		if len(items) != 1 || items[0].ID != "i-19" {
			t.Fatal(items)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

type queueFailureCatalog struct {
	catalog
	failed   func()
	attempts int
}

func (d *queueFailureCatalog) Read(ctx context.Context, fn func(*store.Queries) error) error {
	err := d.catalog.Read(ctx, fn)
	if err != nil && strings.Contains(err.Error(), "jobs_queue") {
		d.attempts++
		d.failed()
	}
	return err
}
func TestReconcileDoesNotSpinOnTemporaryFailure(t *testing.T) {
	t.Parallel()
	f, token, p, _ := managedFixture(t)
	putFile(t, f, token, p.ID, "input.md", "source")
	var j fixtureJob
	if c := f.request("POST", "/api/projects/"+p.ID+"/generate", token, nil, &j); c != 202 {
		t.Fatal(c)
	}
	fixtureSQL(t, f, "DROP INDEX jobs_queue")
	d := &queueFailureCatalog{catalog: f.s.Store, failed: func() {
		for i := 0; i < 100; i++ {
			f.s.notify()
		}
	}}
	f.s.Store = d
	if err := f.s.runQueuedJobs(t.Context()); err == nil {
		t.Fatal("queue SQL failure ignored")
	}
	if d.attempts != 1 || len(f.s.wake) != 0 {
		t.Fatalf("attempts=%d queued notifications=%d", d.attempts, len(f.s.wake))
	}
	if got := storedJob(t, f, j.ID); got.Status != "queued" || got.Run != nil {
		t.Fatal(got)
	}
	fixtureSQL(t, f, "CREATE INDEX jobs_queue ON jobs(created,id) WHERE status='queued'")
	f.s.notify()
	if len(f.s.wake) != 1 {
		t.Fatal("new notification lost")
	}
	<-f.s.wake
	if err := f.s.runQueuedJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := storedJob(t, f, j.ID); got.Status != "running" || got.Run == nil {
		t.Fatal(got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.s.runQueuedJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if d.attempts != 1 {
		t.Fatal("retried without a new trigger")
	}
}

func TestViewContinuationRechecksAuthorization(t *testing.T) {
	t.Parallel()
	o, _ := storageFixture(t)
	sqlWrite(t, o.db, func(q *store.Queries) error {
		if err := q.PutView(store.ViewRow{ID: "v", ProjectID: "p", Slug: "map", Data: sqlTestViewData()}); err != nil {
			return err
		}
		if err := q.PutJob(store.JobRow{ID: "parent", ProjectID: "p", Status: "running", Kind: "generate", GenerationSnapshot: store.NullableString("snapshot"), Data: store.JobData{UserID: "u", Views: []store.ViewResultData{{ViewID: "v", Status: "pending"}}, ViewGeneration: &store.GenerationData{Remaining: 100}}}); err != nil {
			return err
		}
		return q.DeleteProjectMember("p", "u")
	})
	if err := o.advanceViews(t.Context(), "parent"); err != nil {
		t.Fatal(err)
	}
	j := readSQLJob(t, o, "parent")
	if j.Status != "failed" || readSQLChildID(t, o, j.ID) != "" || j.Data.Views[0].Status != "skipped" {
		t.Fatal(j)
	}
	if err := o.db.Read(t.Context(), func(q *store.Queries) error {
		jobs, err := q.ProjectJobs("p")
		if len(jobs) != 1 {
			t.Fatal("revoked user created child", jobs)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectDeletionWaitsForChildCleanupAndRetainsUsage(t *testing.T) {
	t.Parallel()
	o, _ := storageFixture(t)
	child := storageRunning(t, o, "child")
	child.Status = "completed"
	child.Kind = "view"
	child.ParentID = store.NullableString("parent")
	child.Data.ViewTask = &store.ViewTaskData{ViewID: "v1"}
	sqlWrite(t, o.db, func(q *store.Queries) error {
		if err := q.PutJob(child); err != nil {
			return err
		}
		return q.PutJob(store.JobRow{ID: "parent", ProjectID: "p", Status: "running", Kind: "generate", GenerationSnapshot: store.NullableString("w"), Data: store.JobData{UserID: "u", Views: []store.ViewResultData{{ViewID: "v1"}}, ViewGeneration: &store.GenerationData{Remaining: 100}}})
	})
	cost := int64(40)
	status := runner.Status{State: runner.StateStopped, Usage: runner.UsageSample{CostCents: &cost}}
	providerUnavailable := true
	cleanup := func(ctx context.Context, j store.JobRow) error {
		_, err := o.cleanup(ctx, j.ID, status, func(context.Context, runner.Attempt) error {
			if providerUnavailable {
				return fail(503, "provider cleanup pending")
			}
			return nil
		})
		return err
	}
	if err := cleanup(t.Context(), child); err == nil {
		t.Fatal("cleanup failure ignored")
	}
	if err := o.advanceViews(t.Context(), "parent"); err != nil {
		t.Fatal(err)
	}
	parent := readSQLJob(t, o, "parent")
	if parent.Data.ViewGeneration.Remaining != 100 || readSQLChildID(t, o, parent.ID) != "child" {
		t.Fatal("charged before cleanup", parent)
	}
	expectSQLStatus(t, o.deleteProject(t.Context(), "p", cleanup), 503)
	providerUnavailable = false
	cost = 99 // A later provider value must not replace the saved usage.
	if err := o.deleteProject(t.Context(), "p", cleanup); err != nil {
		t.Fatal(err)
	}
	if err := o.db.Read(t.Context(), func(q *store.Queries) error {
		jobs, err := q.ProjectJobs("p")
		if err != nil {
			return err
		}
		if len(jobs) != 0 {
			t.Fatal(jobs)
		}
		usage, err := q.Usage("child")
		if err != nil {
			return err
		}
		if usage.Data.CostCents == nil || *usage.Data.CostCents != 40 {
			t.Fatal(usage)
		}
		audit, err := q.ProjectAudit("p")
		if len(audit) != 0 {
			t.Fatal("deletion left audit", audit)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectDeletionRechecksSessionAfterObjectIO(t *testing.T) {
	t.Parallel()
	f, token, p := orgProject(t)
	putFile(t, f, token, p.ID, "source.md", "deletion evidence")
	objects := &storageTestObjects{Store: f.s.Blobs}
	objects.del = func(ctx context.Context, _ blob.Key) error {
		return f.stateStore.Write(ctx, func(q *store.Queries) error { return q.DeleteSession(auth.Hash(token)) })
	}
	f.s.Blobs, f.s.storage.objects = objects, objects
	if code := f.request("DELETE", "/api/projects/"+p.Namespace, token, nil, nil); code != 401 {
		t.Fatal("revoked session completed deletion", code)
	}
	if err := f.stateStore.Read(t.Context(), func(q *store.Queries) error {
		current, err := q.Project(p.ID)
		if err == nil && !current.Deleting {
			t.Fatal("lost deletion checkpoint")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Resume with a fresh, explicitly authorized owner session.
	fresh := f.login("owner@example.com")
	if code := f.request("DELETE", "/api/projects/"+p.Namespace, fresh, nil, nil); code != 200 {
		t.Fatal(code)
	}
}
