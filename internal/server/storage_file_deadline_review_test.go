package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
)

type delayedOutputRunner struct {
	runner.FileRunner
	collect func() []byte
	err     error
}

func (r delayedOutputRunner) CollectOutput(context.Context, runner.Attempt, runner.Status, int64) ([]byte, error) {
	return r.collect(), r.err
}

func TestFileOutputRechecksDeadlineAfterIO(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		deadline, elapsed  time.Duration
		duringPut, expired bool
		collectErr         error
	}{
		{name: "before cutoff", deadline: 2 * time.Minute, elapsed: 90*time.Second - time.Millisecond},
		{name: "download crosses wait cutoff", deadline: 2 * time.Minute, elapsed: 90 * time.Second, expired: true},
		{name: "download crosses job cutoff", deadline: 45 * time.Second, elapsed: 45 * time.Second, expired: true},
		{name: "PUT crosses wait cutoff", deadline: 2 * time.Minute, elapsed: 90 * time.Second, duringPut: true, expired: true},
		{name: "temporary failure crosses cutoff", deadline: 2 * time.Minute, elapsed: 90 * time.Second, expired: true, collectErr: runner.ErrTemporary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, p, _ := managedFixture(t)
			j := startManaged(t, f, token, p.ID, true)
			now := time.Now().UTC().Truncate(time.Millisecond)
			start := now
			f.s.storage.now = func() time.Time { return now }
			sqlWrite(t, f.stateStore, func(q *store.Queries) error {
				row, err := q.Job(j.ID)
				if err != nil {
					return err
				}
				row.Data.Run.UsesProviderFiles = true
				row.Data.Run.Deadline = start.Add(tc.deadline).UnixMilli()
				return q.PutJob(row)
			})
			f.s.SetRunner(&testRunner{cleanup: func(context.Context, runner.Attempt) error { return nil }})
			if tc.duringPut {
				objects := &storageTestObjects{Store: f.s.Blobs, put: func(_ context.Context, key blob.Key, _ []byte) error {
					if strings.Contains(string(key), "/output-") {
						now = start.Add(tc.elapsed)
					}
					return nil
				}}
				f.s.Blobs, f.s.storage.objects = objects, objects
			}
			output := outputArchive(t, map[string]string{"wiki/index.md": "# Offline", "wiki/log.md": "# Log"})
			r := delayedOutputRunner{err: tc.collectErr, collect: func() []byte {
				if !tc.duringPut {
					now = start.Add(tc.elapsed)
				}
				return output
			}}
			row := readSQLJob(t, f.s.storage, j.ID)
			status := runner.Status{State: runner.StateStopped, Run: sqlAttempt(row).Run}
			if err := f.s.collectAgentOutput(t.Context(), r, row, status); err != nil {
				t.Fatal(err)
			}
			got := readSQLJob(t, f.s.storage, j.ID)
			if tc.expired {
				if got.Status != "failed" || got.OutputHash != nil || store.ValueString(got.Data.Error) != "managed agent output did not become available before the deadline" {
					t.Fatalf("late output was accepted: %+v", got)
				}
			} else if store.ValueString(got.Data.WikiStatus) != "completed" || got.OutputHash == nil {
				t.Fatalf("on-time output was rejected: %+v", got)
			}
		})
	}
}
