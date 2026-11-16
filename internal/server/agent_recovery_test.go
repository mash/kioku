package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/store"
)

func TestManagedRestartEveryPhase(t *testing.T) {
	t.Parallel()
	for _, phase := range []model.ManagedPhase{model.PhasePreparing, model.PhaseAgentRunning, model.PhaseOutputReceived, model.PhaseFinalizing} {
		t.Run(string(phase), func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			putFile(t, f, token, p.ID, "x.md", "snapshot")
			j := startManaged(t, f, token, p.ID, false)
			sendOutput(t, f, ma, j, "Recovered")
			if phase == model.PhaseOutputReceived || phase == model.PhaseFinalizing {
				ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
				objects := f.s.Blobs
				f.s.storage.objects = failWikiPublication{objects}
				deferredPublication(t, f, j)
				f.s.storage.objects = objects
			}
			_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				x := fixtureJobsGet(t, st, j.ID)
				x.Run.Phase = phase
				if phase == model.PhasePreparing {
					x.Run.Ref.ID = ""
					x.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
				}
				fixtureJobsPut(t, st, j.ID, x)
				return nil
			})
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
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
			restored.SetRunner(g.Runner)
			if err := restored.Prepare(); err != nil {
				t.Fatal(err)
			}
			f.stateStore = stateStore
			f.s = restored
			ts := httptest.NewServer(restored.Handler())
			t.Cleanup(ts.Close)
			f.url = ts.URL
			f.client = ts.Client()
			_ = stateStore.Read(context.Background(), func(st *store.Queries) error {
				got := fixtureJobsGet(t, st, j.ID)
				if got.Status != "running" || got.Run.Phase != phase {
					t.Error("restart changed run", got)
				}
				return nil
			})
			got := reconcileManaged(t, f, j)
			want := "completed"
			if got.Status != want {
				t.Fatal(got)
			}
			assertAgentCleanup(t, f, ma, got)
			_ = stateStore.Read(context.Background(), func(st *store.Queries) error {
				if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 123 {
					t.Error("restart lost cost")
				}
				return nil
			})
		})
	}
}

type pauseOutputRead struct {
	blob.Store
	key              blob.Key
	entered, release chan struct{}
	once             sync.Once
}

func (b *pauseOutputRead) Get(ctx context.Context, key blob.Key) ([]byte, error) {
	if key == b.key {
		b.once.Do(func() { close(b.entered); <-b.release })
	}
	return b.Store.Get(ctx, key)
}

func TestManagedTemporaryCreationRecoveryAndNoOp(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	ma.SetError("POST", "/v1/sessions", 503)
	j := startManaged(t, f, token, p.ID, false)
	if j.Run.Phase != model.PhasePreparing {
		t.Fatal(j)
	}
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		x := fixtureJobsGet(t, st, j.ID)
		x.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
		fixtureJobsPut(t, st, j.ID, x)
		return nil
	})
	if got := reconcileManaged(t, f, j); got.Status != "failed" {
		t.Fatal(got)
	}
	ma.SetError("POST", "/v1/sessions", 0)
	j = startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Wiki")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	reconcileManaged(t, f, j)
	j = startManaged(t, f, token, p.ID, false)
	if !j.Unchanged || j.Status != "completed" || j.Run != nil {
		t.Fatal("unchanged job invoked a Wiki runner", j)
	}
	j = startManaged(t, f, token, p.ID, true)
	if j.Unchanged || j.Run == nil {
		t.Fatal("full skipped agent", j)
	}
}
func TestManagedWorkerSessionAndProjectLimits(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	f.s.Config.AgentMaxSessions = 2
	putFile(t, f, token, p.ID, "x.md", "evidence")
	projects := []fixtureProject{p}
	for _, name := range []string{"other", "third"} {
		var p fixtureProject
		if c := f.request("POST", "/api/projects", token, map[string]string{"name": "acme/" + name}, &p); c != 201 {
			t.Fatal(c)
		}
		putFile(t, f, token, p.ID, "x.md", "evidence")
		projects = append(projects, p)
	}
	var first fixtureJob
	f.request("POST", "/api/projects/"+p.ID+"/generate", token, nil, &first)
	// Persist an independent queued request to exercise the worker's serialization.
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		second := first
		second.ID = "queued-same-project"
		second.Created = first.Created.Add(time.Millisecond)
		fixtureJobsPut(t, st, second.ID, second)
		return nil
	})
	for _, p := range projects[1:] {
		f.request("POST", "/api/projects/"+p.ID+"/generate", token, nil, nil)
	}
	started := make(chan string, 10)
	ma.OnSession(func(id string) { started <- id })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.s.RunWorker(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not start slots")
		}
	}
	// A full worker wake/tick must still leave the third project and second request queued.
	f.s.notify()
	time.Sleep(100 * time.Millisecond)
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		running := 0
		seen := map[string]bool{}
		for _, j := range fixtureJobsList(t, st, projects[0].ID, projects[1].ID, projects[2].ID) {
			if j.Status == "running" && j.Run != nil {
				running++
				if seen[j.ProjectID] {
					t.Error("same project concurrent")
				}
				seen[j.ProjectID] = true
			}
		}
		if running != 2 {
			t.Error("session limit", running)
		}
		if fixtureJobsGet(t, st, "queued-same-project").Status != "queued" {
			t.Error("same-project request started")
		}
		return nil
	})
	select {
	case id := <-started:
		t.Error("exceeded sessions", id)
	default:
	}
}
