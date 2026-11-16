package server

import (
	"context"
	"strconv"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

type workerFailureCatalog struct {
	catalog
	failures int
}

func (d *workerFailureCatalog) Write(ctx context.Context, fn func(*store.Queries) error) error {
	err := d.catalog.Write(ctx, fn)
	if err != nil {
		d.failures++
	}
	return err
}

func TestWorkerFailureDoesNotBlockOtherProjects(t *testing.T) {
	t.Parallel()
	for _, continuation := range []bool{false, true} {
		t.Run(strconv.FormatBool(continuation), func(t *testing.T) {
			f, human, bad, _ := managedFixture(t)
			good := consentProject(t, f, human, "other")
			putFile(t, f, human, good.ID, "source.md", "source")
			fixtureCheck(t, f.stateStore.Write(t.Context(), func(q *store.Queries) error {
				badJob := store.JobRow{ID: "bad", ProjectID: bad.ID, Status: "queued", Kind: "generate"}
				// Use the authenticated owner's ID without depending on generated fixture IDs.
				session, err := q.Session(auth.Hash(human))
				if err != nil {
					return err
				}
				badJob.Data.UserID = session.UserID
				if continuation {
					badJob.Status = "running"
					snapshot := ""
					badJob.GenerationSnapshot = &snapshot
					badJob.Data.ViewGeneration = &store.GenerationData{Remaining: 100}
					goodParent := badJob
					goodParent.ID, goodParent.ProjectID = "good-parent", good.ID
					if err := q.PutJob(goodParent); err != nil {
						return err
					}
				}
				if err := q.PutJob(badJob); err != nil {
					return err
				}
				return q.PutJob(store.JobRow{ID: "good", ProjectID: good.ID, Status: "queued", Kind: "generate", Created: 1, Data: store.JobData{UserID: session.UserID}})
			}))
			restore := fixtureSQLFault(t, f, "jobs", "UPDATE", "NEW.id='bad'")
			d := &workerFailureCatalog{catalog: f.stateStore}
			f.s.Store, f.s.storage.db = d, d
			if err := f.s.runQueuedJobs(t.Context()); err == nil {
				t.Fatal("job-specific SQL failure was hidden")
			}
			if d.failures != 1 {
				t.Fatalf("failed job was retried in the same pass: %d failures", d.failures)
			}
			if job := storedJob(t, f, "good"); job.Status != "running" || job.Run == nil {
				t.Fatal("unrelated project was blocked", job)
			}
			badStatus := "queued"
			if continuation {
				badStatus = "running"
			}
			if job := storedJob(t, f, "bad"); job.Status != badStatus || job.Run != nil {
				t.Fatal("failed job was changed", job)
			}
			restore()
			fixtureCheck(t, f.s.runQueuedJobs(t.Context()))
			if job := storedJob(t, f, "bad"); job.Status != "completed" {
				t.Fatal("failed job was not retried on the next pass", job)
			}
		})
	}
}
