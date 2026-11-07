package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	for _, phase := range []model.ManagedPhase{model.PhasePreparing, model.PhaseAgentRunning, model.PhaseOutputReceived, model.PhaseFinalizing} {
		t.Run(string(phase), func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			putFile(t, f, token, p.ID, "x.md", "snapshot")
			j := startManaged(t, f, token, p.ID, false)
			if phase == model.PhaseOutputReceived || phase == model.PhaseFinalizing {
				sendOutput(t, f, ma, j, "Recovered")
			}
			_ = f.db.Update(func(st *model.State) error {
				x := st.Jobs[j.ID]
				x.Run.Phase = phase
				if phase == model.PhasePreparing {
					x.Run.Ref.ID = ""
					x.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
				}
				st.Jobs[j.ID] = x
				return nil
			})
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
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
			g, err := runners.Load(f.runnerConfig, nil)
			if err != nil {
				t.Fatal(err)
			}
			restored.SetRunners(g)
			if err := restored.Prepare(); err != nil {
				t.Fatal(err)
			}
			f.db = db
			f.s = restored
			ts := httptest.NewServer(restored.Handler())
			t.Cleanup(ts.Close)
			f.url = ts.URL
			f.client = ts.Client()
			_ = db.View(func(st model.State) error {
				got := st.Jobs[j.ID]
				if got.Status != "running" || got.Run.Phase != phase {
					t.Error("restart changed run", got)
				}
				return nil
			})
			got := reconcileManaged(t, f, j)
			want := "failed"
			if phase == model.PhaseOutputReceived || phase == model.PhaseFinalizing {
				want = "completed"
			}
			if got.Status != want {
				t.Fatal(got)
			}
			assertAgentCleanup(t, f, ma, got)
			_ = db.View(func(st model.State) error {
				if costOf(st.Jobs[j.ID].Run.CostCents) != 123 {
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
func TestManagedFinalizingPinsOutputHash(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Selected version")
	j = storedJob(t, f, j.ID)
	b := &pauseOutputRead{Store: f.s.Blobs, key: j.Run.OutputKey, entered: make(chan struct{}), release: make(chan struct{})}
	f.s.Blobs = b
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	done := make(chan error, 1)
	go func() { done <- f.s.reconcileAgentJob(context.Background(), j) }()
	<-b.entered
	c, body := agentRequest(t, f, j, ma.Credential(j.Run.Ref.Resources["vault_id"]), "PUT", "output", outputArchive(t, map[string]string{"wiki/index.md": "Replacement"}))
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c != 409 {
		t.Fatal(c, string(body))
	}
	var snap model.Snapshot
	f.request("GET", "/api/projects/"+p.ID+"/snapshot", token, nil, &snap)
	if !strings.Contains(snap.Files["wiki/index.md"], "Selected version") || strings.Contains(snap.Files["wiki/index.md"], "Replacement") {
		t.Fatal(snap)
	}
}
func TestManagedTemporaryCreationRecoveryAndNoOp(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	ma.SetError("POST", "/v1/sessions", 503)
	j := startManaged(t, f, token, p.ID, false)
	if j.Run.Phase != model.PhasePreparing {
		t.Fatal(j)
	}
	_ = f.db.Update(func(st *model.State) error {
		x := st.Jobs[j.ID]
		x.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
		st.Jobs[j.ID] = x
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
	n := len(ma.Requests())
	j = startManaged(t, f, token, p.ID, false)
	if !j.Unchanged || j.Status != "completed" || len(ma.Requests()) != n {
		t.Fatal("unchanged invoked agent", j)
	}
	j = startManaged(t, f, token, p.ID, true)
	if j.Unchanged || j.Run == nil {
		t.Fatal("full skipped agent", j)
	}
}
func TestManagedWorkerSessionAndProjectLimits(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	f.s.Config.AgentMaxSessions = 2
	putFile(t, f, token, p.ID, "x.md", "evidence")
	projects := []model.Project{p}
	for _, name := range []string{"other", "third"} {
		var p model.Project
		if c := f.request("POST", "/api/projects", token, map[string]string{"name": "acme/" + name}, &p); c != 201 {
			t.Fatal(c)
		}
		putFile(t, f, token, p.ID, "x.md", "evidence")
		projects = append(projects, p)
	}
	var first model.Job
	f.request("POST", "/api/projects/"+p.ID+"/generate", token, nil, &first)
	// Persist an independent queued request to exercise the worker's serialization.
	_ = f.db.Update(func(st *model.State) error {
		second := first
		second.ID = "queued-same-project"
		second.Created = first.Created.Add(time.Millisecond)
		second.Full = true
		st.Jobs[second.ID] = second
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
	_ = f.db.View(func(st model.State) error {
		running := 0
		seen := map[string]bool{}
		for _, j := range st.Jobs {
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
		if st.Jobs["queued-same-project"].Status != "queued" {
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
