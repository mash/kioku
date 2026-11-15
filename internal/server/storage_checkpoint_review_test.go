package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
)

type checkpointFileRunner struct {
	runner.FileRunner
	start func(context.Context, runner.RunSpec) (runner.RunRef, error)
}

func (r checkpointFileRunner) Provider() string            { return "anthropic" }
func (r checkpointFileRunner) FilePaths() (string, string) { return "input.tar.gz", "output.tar.gz" }
func (r checkpointFileRunner) Start(ctx context.Context, spec runner.RunSpec) (runner.RunRef, error) {
	return r.start(ctx, spec)
}

type checkpointFailureCatalog struct {
	catalog
	failure    error
	reconnects int
}

func (d *checkpointFailureCatalog) Write(context.Context, func(*store.Queries) error) error {
	return d.failure
}
func (d *checkpointFailureCatalog) ResolveCommit(ctx context.Context, fn func(*store.Queries) error) error {
	d.reconnects++
	return d.catalog.ResolveCommit(ctx, fn)
}

func TestFileCheckpointPreservesDatabaseFailure(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(fmt.Sprintf("fatal=%t", fatal), func(t *testing.T) {
			f, token, p, _ := managedFixture(t)
			j := managedJob(t, f, token, p.ID, true)
			cause := errors.New("checkpoint failure")
			var failure error = &store.UnknownCommit{Cause: cause}
			if fatal {
				failure = &store.FatalDatabaseError{Cause: cause}
			}
			d := &checkpointFailureCatalog{catalog: f.stateStore, failure: failure}
			f.s.SetRunners(testRunners{Runners: f.s.runners, run: checkpointFileRunner{start: func(ctx context.Context, spec runner.RunSpec) (runner.RunRef, error) {
				// Input preparation succeeds; only the resource checkpoint fails.
				f.s.Store, f.s.storage.db = d, d
				return runner.StartFileSession(ctx, spec, "anthropic", func(context.Context, string, []byte) (string, error) {
					t.Fatal("upload ran after a failed checkpoint")
					return "", nil
				}, func(context.Context, string, func(string) error) (string, error) {
					t.Fatal("session started after a failed checkpoint")
					return "", nil
				})
			}}})
			err := f.s.startAgentJob(t.Context(), fixtureRow(j))
			if !errors.Is(err, failure) {
				t.Fatalf("database failure was hidden: %v", err)
			}
			_ = f.s.databaseError(t.Context(), err)
			if fatal {
				select {
				case <-f.s.fatal:
				default:
					t.Fatal("fatal checkpoint failure did not stop the server")
				}
			} else if d.reconnects != 1 {
				t.Fatalf("ambiguous commit was not resolved: %d reconnects", d.reconnects)
			}
			got := storedJob(t, f, j.ID)
			if got.Status != "running" || got.Run == nil || got.Run.Ref.Resources["upload_started"] == "true" {
				t.Fatalf("checkpoint failure changed durable run state: %+v", got)
			}
		})
	}
}
