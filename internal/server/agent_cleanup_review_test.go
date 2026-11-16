package server

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/runner/claudemanaged"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/store"
)

type unavailableCleanupClient struct{ claudemanaged.Client }

func (unavailableCleanupClient) ArchiveSession(context.Context, string) error {
	return claudemanaged.ErrTemporary
}
func (unavailableCleanupClient) ArchiveVault(context.Context, string) error {
	return claudemanaged.ErrTemporary
}

type unavailableCleanupStore struct{ blob.Store }

func (b unavailableCleanupStore) Delete(ctx context.Context, key blob.Key) error {
	if strings.HasSuffix(string(key), ".tar.gz") {
		return errors.New("cleanup interrupted")
	}
	return b.Store.Delete(ctx, key)
}
func restartManagedFixture(t *testing.T, f *fixture) {
	t.Helper()
	old := f.s
	dir := filepath.Dir(old.Blobs.(local.Store).Dir)
	if err := f.stateStore.Close(); err != nil {
		t.Fatal(err)
	}
	stateStore, err := openFixtureDatabase(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stateStore.Close() })
	restored := New(old.Config, stateStore, old.Blobs, old.Cipher)
	g, err := runners.Load(f.runnerConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	restored.SetRunners(g)
	if err := restored.Prepare(); err != nil {
		t.Fatal(err)
	}
	f.s = restored
	f.stateStore = stateStore
	ts := httptest.NewServer(restored.Handler())
	t.Cleanup(ts.Close)
	f.url = ts.URL
	f.client = ts.Client()
	restored.Config.BaseURL = ts.URL
}
func TestManagedTerminalCleanupSurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			putFile(t, f, token, p.ID, "x.md", "evidence")
			j := startManaged(t, f, token, p.ID, false)
			if success {
				sendOutput(t, f, ma, j, "Complete")
			}
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
			client, objects := f.runnerConfig.ManagedAgents, f.s.Blobs
			f.runnerConfig.ManagedAgents = unavailableCleanupClient{client}
			f.setRunners()
			f.s.Blobs = unavailableCleanupStore{objects}
			f.s.storage.objects = f.s.Blobs
			j = reconcileManaged(t, f, j)
			want := "completed"
			if !success {
				want = "failed"
			}
			if j.Status != want && !(success && j.Status == "running" && j.WikiStatus == "completed") {
				t.Fatal(j)
			}
			f.runnerConfig.ManagedAgents = client
			f.setRunners()
			f.s.Blobs = objects
			f.s.storage.objects = f.s.Blobs
			restartManagedFixture(t, f)
			if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
				t.Fatal(err)
			}
			j = f.completeFixtureViews(storedJob(t, f, j.ID))
			assertAgentCleanup(t, f, ma, j)
			_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
				if fixtureJobsGet(t, st, j.ID).Status != want || costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 123 {
					t.Error("cleanup changed outcome")
				}
				return nil
			})
		})
	}
}

func TestManagedPendingCleanupPreservesDeletionAndAllowsNewGeneration(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 100)
	ma.SetError("POST", "/v1/sessions/"+j.Run.Ref.ID+"/archive", 503)
	j = reconcileManaged(t, f, j)
	if j.Run.CleanupComplete {
		t.Fatal("failed cleanup marked complete")
	}
	next := startManaged(t, f, token, p.ID, false)
	if next.ID == j.ID || next.Status != "running" || next.Run.Attempt.ID == j.Run.Attempt.ID {
		t.Fatal(next)
	}
	if c := f.request("DELETE", "/api/projects/"+p.ID, token, nil, nil); c != 503 {
		t.Fatal("deletion lost cleanup", c)
	}
	if !fixtureProjectsGet(t, f, p.ID).Deleting {
		t.Fatal("deletion intent not durable")
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/generate", token, nil, nil); c != 409 {
		t.Fatal("deleting project accepted new work", c)
	}
	if old := storedJob(t, f, j.ID); old.Status != "failed" || old.Run.CleanupComplete {
		t.Fatal(old)
	}

}
