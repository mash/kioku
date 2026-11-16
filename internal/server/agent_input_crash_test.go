package server

import (
	"context"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/blob"
)

type crashAfterInputWrite struct{ blob.Store }

func (b crashAfterInputWrite) Put(ctx context.Context, key blob.Key, body []byte) error {
	if err := b.Store.Put(ctx, key, body); err != nil {
		return err
	}
	if strings.HasSuffix(string(key), "/input.tar.gz") {
		panic("simulated process crash after input write")
	}
	return nil
}
func TestManagedInputWriteCrashLeavesNoRunAndDeletesOrphan(t *testing.T) {
	t.Parallel()
	f, token, p, _ := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "must be erased")
	j := managedJob(t, f, token, p.ID, false)
	objects := f.s.Blobs
	f.s.Blobs = crashAfterInputWrite{objects}
	f.s.storage.objects = f.s.Blobs
	func() {
		defer func() {
			if recover() == nil {
				t.Error("crash point not reached")
			}
		}()
		_ = f.s.startAgentJob(context.Background(), fixtureRow(j))
	}()
	f.s.Blobs = objects
	f.s.storage.objects = f.s.Blobs
	restartManagedFixture(t, f)
	// E13: no Run, assignment, credential or provider call exists before input commit.
	if got := storedJob(t, f, j.ID); got.Status != "queued" || got.Run != nil {
		t.Fatal(got)
	}
	if code := f.request("DELETE", "/api/projects/"+p.ID, token, nil, nil); code != 200 {
		t.Fatal(code)
	}
	keys, err := objects.List(context.Background(), fixtureJobPrefix(j))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("deleted source survived as an orphan input: %v", keys)
	}
}
