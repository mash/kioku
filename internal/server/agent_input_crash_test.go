package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"

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
func TestManagedInputWriteCrashRetainsCleanupIdentity(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "must be erased")
	j := managedJob(t, f, token, p.ID, false)
	objects := f.s.Blobs
	f.s.Blobs = crashAfterInputWrite{objects}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("crash point not reached")
			}
		}()
		_ = f.s.startAgentJob(context.Background(), j)
	}()
	f.s.Blobs = objects
	restartManagedFixture(t, f)
	if code := f.request("DELETE", "/api/projects/"+p.ID, token, nil, nil); code != 503 {
		t.Fatal(code)
	}
	_ = f.db.Update(func(st *model.State) error {
		j := st.Jobs[j.ID]
		j.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
		st.Jobs[j.ID] = j
		return nil
	})
	if err := f.s.reconcileAgentJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code := f.request("DELETE", "/api/projects/"+p.ID, token, nil, nil); code != 200 {
		t.Fatal(code)
	}
	keys, err := objects.List(context.Background(), agentJobPrefix(j))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("deleted source survived as an orphan input: %v", keys)
	}
}
