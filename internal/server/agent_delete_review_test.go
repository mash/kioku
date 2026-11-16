package server

import (
	"context"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func TestManagedDeleteRecoversUnrecordedSession(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(j.Run.Ref.ID, "running", "", 123)
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		x := fixtureJobsGet(t, st, j.ID)
		x.Run.Ref.ID = ""
		x.Run.Phase = model.PhasePreparing
		fixtureJobsPut(t, st, x.ID, x)
		return nil
	})
	path := "/api/projects/" + p.ID
	if c := f.request("DELETE", path, token, nil, nil); c != 503 {
		t.Fatal(c)
	}
	for range 2 {
		if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if c := f.request("DELETE", path, token, nil, nil); c != 200 {
		t.Fatal(c)
	}
	if !ma.Session(j.Run.Ref.ID).Interrupted || !ma.Session(j.Run.Ref.ID).Archived {
		t.Error("unrecorded session orphaned")
	}
}

func TestManagedDeleteDefersProviderRejections(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"stop", "cleanup"} {
		t.Run(operation, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			j := startManaged(t, f, token, p.ID, true)
			endpoint := "/v1/sessions/" + j.Run.Ref.ID + "/events"
			if operation == "cleanup" {
				ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
				endpoint = "/v1/sessions/" + j.Run.Ref.ID + "/archive"
			}
			ma.SetError("POST", endpoint, 403)
			path := "/api/projects/" + p.ID
			if code := f.request("DELETE", path, token, nil, nil); code != 503 {
				t.Fatalf("provider %s rejection must leave deletion pending: %d", operation, code)
			}
			if !fixtureProjectsGet(t, f, p.ID).Deleting || storedJob(t, f, j.ID).Run.CleanupComplete {
				t.Fatal("provider failure lost pending deletion or cleanup")
			}
			ma.SetError("POST", endpoint, 0)
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
			if code := f.request("DELETE", path, token, nil, nil); code != 200 {
				t.Fatalf("deletion did not resume after provider recovery: %d", code)
			}
			fixtureCheck(t, f.stateStore.Read(t.Context(), func(q *store.Queries) error {
				usage, err := q.Usage(j.ID)
				if err == nil && (usage.Data.CostCents == nil || *usage.Data.CostCents != 123) {
					t.Fatal("deletion lost usage", usage)
				}
				return err
			}))
		})
	}
}

func TestManagedDeleteStorageFailureRemainsInternalError(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	j := startManaged(t, f, token, p.ID, true)
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	objects := f.s.Blobs
	f.s.Blobs = unavailableCleanupStore{objects}
	f.s.storage.objects = f.s.Blobs
	path := "/api/projects/" + p.ID
	if code := f.request("DELETE", path, token, nil, nil); code != 500 {
		t.Fatalf("storage cleanup failure must remain a storage error: %d", code)
	}
	f.s.Blobs = objects
	f.s.storage.objects = objects
	if code := f.request("DELETE", path, token, nil, nil); code != 200 {
		t.Fatalf("deletion did not resume after storage recovery: %d", code)
	}
}

func TestRemovedSourceDeletionRouteLeavesGenerationRunning(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	if c := f.request("DELETE", "/api/projects/"+p.ID+"/sources/missing", token, nil, nil); c != 404 && c != 405 {
		t.Fatal(c)
	}
	if got := storedJob(t, f, j.ID); got.Status != "running" {
		t.Error("404 canceled generation", got.Status)
	}
	if ma.Session(j.Run.Ref.ID).Interrupted {
		t.Error("404 interrupted session")
	}
}
