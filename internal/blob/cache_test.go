package blob

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

type cacheRemote struct {
	mu               sync.Mutex
	data             map[string][]byte
	fail             bool
	gets             int
	entered, release chan struct{}
}

func (r *cacheRemote) Put(_ context.Context, k string, b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("remote unavailable")
	}
	r.data[k] = append([]byte(nil), b...)
	return nil
}
func (r *cacheRemote) Get(ctx context.Context, k string) ([]byte, error) {
	r.mu.Lock()
	r.gets++
	b, ok := r.data[k]
	b = append([]byte(nil), b...)
	entered, release := r.entered, r.release
	r.entered = nil
	r.mu.Unlock()
	if entered != nil {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if !ok {
		return nil, HTTPError{Status: 404}
	}
	return b, nil
}
func (r *cacheRemote) Delete(_ context.Context, k string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.data, k)
	return nil
}
func (r *cacheRemote) List(context.Context, string) ([]string, error) { return nil, nil }

func TestCacheRequiresRemoteDurabilityAndIgnoresOldDiskOnRestart(t *testing.T) {
	ctx := context.Background()
	remote := &cacheRemote{data: map[string][]byte{}, fail: true}
	dir := t.TempDir()
	cache := &Cache{Remote: remote, Dir: dir, MaxBytes: 4}
	key := "projects/p/wiki/index.md"
	if err := cache.Put(ctx, key, []byte("old")); err == nil {
		t.Fatal("accepted a local-only write")
	}
	if _, err := os.Stat(cache.path(key)); !os.IsNotExist(err) {
		t.Fatal("failed remote write populated cache")
	}
	remote.fail = false
	if err := cache.Put(ctx, key, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(ctx, key, []byte("new")); err != nil {
		t.Fatal(err)
	}
	restarted := &Cache{Remote: remote, Dir: dir, MaxBytes: 4}
	b, err := restarted.Get(ctx, key)
	if err != nil || string(b) != "new" {
		t.Fatal("stale disk cache overrode R2", string(b), err)
	}
	if err = restarted.Put(ctx, "projects/p/wiki/other.md", []byte("more")); err != nil {
		t.Fatal(err)
	}
	if restarted.size > 4 {
		t.Fatal("cache size limit exceeded", restarted.size)
	}
	meta := "projects/p/.kioku/state.json"
	if err = remote.Put(ctx, meta, []byte("one")); err != nil {
		t.Fatal(err)
	}
	restarted.Get(ctx, meta)
	remote.Put(ctx, meta, []byte("two"))
	b, err = restarted.Get(ctx, meta)
	if err != nil || string(b) != "two" {
		t.Fatal("metadata was cached", string(b), err)
	}
}

func TestLateCacheReadCannotReplaceNewerWrite(t *testing.T) {
	ctx := context.Background()
	key := "projects/p/wiki/index.md"
	remote := &cacheRemote{data: map[string][]byte{key: []byte("old")}, entered: make(chan struct{}), release: make(chan struct{})}
	entered, release := remote.entered, remote.release
	cache := &Cache{Remote: remote, Dir: t.TempDir()}
	done := make(chan error, 1)
	go func() { _, err := cache.Get(ctx, key); done <- err }()
	<-entered
	if err := cache.Put(ctx, key, []byte("new")); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b, err := cache.Get(ctx, key)
	if err != nil || string(b) != "new" {
		t.Fatal("late fetch replaced new cache", string(b), err)
	}
}
