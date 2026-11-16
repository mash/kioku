package server

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
)

func TestRevokedAgentCannotPublishAnAlreadyRunningGeneration(t *testing.T) {
	t.Parallel()
	f, human, p, ma := managedFixture(t)
	id, token := testAgent(t, f, human, p.ID, "write")
	putFile(t, f, token, p.ID, "evidence.md", "Evidence")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Summary")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		v := fixtureAgentTokensGet(t, st, p.ID, id)
		v.RevokedAt = time.Now().UTC()
		fixtureAgentTokensPut(t, st, id, v)
		return nil
	})
	if got := reconcileManaged(t, f, j); got.Status != "failed" {
		t.Fatal(got)
	}
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if fixtureProjectsGet(t, st, p.ID).Revision != "" || len(fixtureRevisionsList(t, st, p.ID)) != 0 {
			t.Error("revoked job published")
		}
		if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 123 {
			t.Error("actual consumed cost lost")
		}
		return nil
	})
}

func TestRunWorkerContinuesAfterPermanentStartFailure(t *testing.T) {
	t.Parallel()
	for _, state := range []runner.State{runner.StateStopped, runner.StateNone} {
		t.Run(fmt.Sprint(state), func(t *testing.T) {
			f, token, p, _ := managedFixture(t)
			f.s.Config.AgentMaxSessions = 2
			var other fixtureProject
			if code := f.request("POST", "/api/projects", token, map[string]string{"name": "acme/other"}, &other); code != 201 {
				t.Fatal(code)
			}
			started := make(chan string, 2)
			var confirmed atomic.Bool
			f.s.SetRunners(testRunners{Runners: f.s.runners, run: &testRunner{
				start: func(_ context.Context, spec runner.RunSpec) (runner.RunRef, error) {
					started <- spec.Key.JobID
					if spec.Key.ProjectID == p.ID {
						return runner.RunRef{}, errors.New("permanent start failure")
					}
					return runner.RunRef{ID: spec.Key.JobID}, nil
				},
				inspect: func(_ context.Context, a runner.Attempt) (runner.Status, error) {
					if a.Key.ProjectID == p.ID && confirmed.Load() {
						return runner.Status{State: state}, nil
					}
					return runner.Status{State: runner.StateUnknown}, nil
				},
				stop:    func(context.Context, runner.Attempt) error { return nil },
				cleanup: func(context.Context, runner.Attempt) error { return nil },
			}})
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- f.s.RunWorker(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("worker did not stop")
				}
			})
			queue := func(pid string) fixtureJob {
				t.Helper()
				putFile(t, f, token, pid, "worker.md", "Evidence")
				var j fixtureJob
				if code := f.request("POST", "/api/projects/"+pid+"/generate", token, nil, &j); code != 202 {
					t.Fatal(code)
				}
				select {
				case id := <-started:
					if id != j.ID {
						t.Fatalf("started %s, want %s", id, j.ID)
					}
				case err := <-done:
					done <- err
					t.Fatalf("worker exited: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("worker did not start job")
				}
				return j
			}
			first := queue(p.ID)
			second := queue(other.ID)
			// Starting the second job proves the worker handled the first Start error.
			pending := storedJob(t, f, first.ID)
			if pending.Status != "running" || pending.Run.AbortReason != "permanent start failure" {
				t.Fatalf("pending failure: %+v", pending)
			}
			if got := storedJob(t, f, second.ID); got.Status != "running" {
				t.Fatalf("second job: %+v", got)
			}
			if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				j := fixtureJobsGet(t, st, first.ID)
				j.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
				fixtureJobsPut(t, st, j.ID, j)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			confirmed.Store(true)
			if got := reconcileManaged(t, f, pending); got.Status != "failed" || got.Error != "permanent start failure" {
				t.Fatalf("failed job: %+v", got)
			}
			select {
			case err := <-done:
				done <- err
				t.Fatalf("worker exited: %v", err)
			default:
			}
		})
	}
}
