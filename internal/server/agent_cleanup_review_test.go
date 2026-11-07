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
	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/store"
)

type unavailableCleanupClient struct{ managedagents.Client }

func (unavailableCleanupClient) ArchiveSession(context.Context, string) error {
	return managedagents.ErrTemporary
}
func (unavailableCleanupClient) ArchiveVault(context.Context, string) error {
	return managedagents.ErrTemporary
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
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	restored := New(old.Config, db, old.Blobs, old.Cipher)
	g, err := runners.Load(f.runnerConfig, db, old.Cipher)
	if err != nil {
		t.Fatal(err)
	}
	restored.SetRunners(g)
	if err := restored.Prepare(); err != nil {
		t.Fatal(err)
	}
	f.s = restored
	f.db = db
	ts := httptest.NewServer(restored.Handler())
	t.Cleanup(ts.Close)
	f.url = ts.URL
	f.client = ts.Client()
	restored.Config.BaseURL = ts.URL
}
func TestManagedTerminalCleanupSurvivesRestart(t *testing.T) {
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
			j = reconcileManaged(t, f, j)
			want := "completed"
			if !success {
				want = "failed"
			}
			if j.Status != want {
				t.Fatal(j)
			}
			f.runnerConfig.ManagedAgents = client
			f.setRunners()
			f.s.Blobs = objects
			restartManagedFixture(t, f)
			if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertAgentCleanup(t, f, ma, j)
			_ = f.db.View(func(st model.State) error {
				if st.Jobs[j.ID].Status != want || costOf(st.Jobs[j.ID].Run.CostCents) != 123 {
					t.Error("cleanup changed outcome")
				}
				return nil
			})
		})
	}
}

func TestManagedPendingCleanupRetainsRetryAndDeletion(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	put := putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 100)
	ma.SetError("POST", "/v1/sessions/"+j.Run.Ref.ID+"/archive", 503)
	j = reconcileManaged(t, f, j)
	if j.Run.CleanupComplete {
		t.Fatal("failed cleanup marked complete")
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/jobs/"+j.ID+"/retry", token, nil, nil); c != 503 {
		t.Fatal("retry lost cleanup", c)
	}
	if c := f.request("DELETE", "/api/projects/"+p.ID+"/sources/"+put.SourceIDs[0], token, nil, nil); c != 503 {
		t.Fatal("deletion lost cleanup", c)
	}
	_ = f.db.View(func(st model.State) error {
		if st.Jobs[j.ID].Run == nil || st.Sources[put.SourceIDs[0]].ID == "" {
			t.Error("cleanup state discarded")
		}
		return nil
	})
	ma.SetError("POST", "/v1/sessions/"+j.Run.Ref.ID+"/archive", 0)
	reconcileManaged(t, f, j)
	next := retryManagedJob(t, f, token, j)
	if next.Status != "running" || next.Run.Attempt.ID == j.Run.Attempt.ID {
		t.Fatal(next)
	}
}
