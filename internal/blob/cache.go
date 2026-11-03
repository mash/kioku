package blob

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mash/kioku/internal/store"
)

// Cache is a disposable, bounded working copy. Remote writes finish before a
// successful Put; no dirty local file is ever the only durable copy. A fresh
// instance trusts no old cache files, so restoring the VPS cannot roll R2 back.
type Cache struct {
	Remote   Store
	Dir      string
	MaxBytes int64
	mu       sync.Mutex
	entries  map[string]cacheEntry
	epochs   map[string]uint64
	size     int64
}
type cacheEntry struct {
	hash  string
	bytes int64
	used  time.Time
}

func (c *Cache) bypass(key string) bool { return strings.Contains(key, "/.kioku/") }
func (c *Cache) init() {
	if c.entries == nil {
		c.entries = map[string]cacheEntry{}
		c.epochs = map[string]uint64{}
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 256 << 20
	}
}
func (c *Cache) path(key string) string { return filepath.Join(c.Dir, hash([]byte(key))) }
func (c *Cache) remember(key string, b []byte) {
	if int64(len(b)) > c.MaxBytes {
		return
	}
	if old, ok := c.entries[key]; ok {
		c.size -= old.bytes
		delete(c.entries, key)
	}
	for c.size+int64(len(b)) > c.MaxBytes {
		victim := ""
		var oldest time.Time
		for k, e := range c.entries {
			if victim == "" || e.used.Before(oldest) {
				victim = k
				oldest = e.used
			}
		}
		if victim == "" {
			break
		}
		_ = os.Remove(c.path(victim))
		c.size -= c.entries[victim].bytes
		delete(c.entries, victim)
	}
	if err := os.MkdirAll(c.Dir, 0700); err != nil {
		return
	}
	if err := store.AtomicWrite(c.path(key), b, 0600); err != nil {
		return
	}
	c.entries[key] = cacheEntry{hash: hash(b), bytes: int64(len(b)), used: time.Now()}
	c.size += int64(len(b))
}
func (c *Cache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.bypass(key) {
		return c.Remote.Get(ctx, key)
	}
	c.mu.Lock()
	c.init()
	epoch := c.epochs[key]
	if e, ok := c.entries[key]; ok {
		b, err := os.ReadFile(c.path(key))
		if err == nil && hash(b) == e.hash {
			e.used = time.Now()
			c.entries[key] = e
			c.mu.Unlock()
			return b, nil
		}
		c.size -= e.bytes
		delete(c.entries, key)
	}
	c.mu.Unlock()
	b, err := c.Remote.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epochs[key] == epoch {
		c.remember(key, b)
	}
	return b, nil
}
func (c *Cache) Put(ctx context.Context, key string, b []byte) error {
	if err := c.Remote.Put(ctx, key, b); err != nil {
		return err
	}
	if c.bypass(key) {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	c.epochs[key]++
	c.remember(key, b)
	return nil
}
func (c *Cache) Delete(ctx context.Context, key string) error {
	if err := c.Remote.Delete(ctx, key); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	c.epochs[key]++
	if e, ok := c.entries[key]; ok {
		c.size -= e.bytes
		delete(c.entries, key)
	}
	_ = os.Remove(c.path(key))
	return nil
}
func (c *Cache) List(ctx context.Context, prefix string) ([]string, error) {
	return c.Remote.List(ctx, prefix)
}
func (c *Cache) EnsureProject(ctx context.Context, id string) error {
	if m, ok := c.Remote.(interface {
		EnsureProject(context.Context, string) error
	}); ok {
		return m.EnsureProject(ctx, id)
	}
	return nil
}
func (c *Cache) DeleteProject(ctx context.Context, id string) error {
	if m, ok := c.Remote.(interface {
		DeleteProject(context.Context, string) error
	}); ok {
		return m.DeleteProject(ctx, id)
	}
	return nil
}
func (c *Cache) Bucket(id string) string {
	if m, ok := c.Remote.(interface{ Bucket(string) string }); ok {
		return m.Bucket(id)
	}
	return ""
}
func (c *Cache) RegisterProjectBucket(id, bucket string) {
	if m, ok := c.Remote.(interface{ RegisterProjectBucket(string, string) }); ok {
		m.RegisterProjectBucket(id, bucket)
	}
}

var _ Store = (*Cache)(nil)

// PruneUnused is called only after the catalog's process lock has been acquired.
func (c *Cache) PruneUnused() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	keep := map[string]bool{}
	for key := range c.entries {
		keep[filepath.Base(c.path(key))] = true
	}
	entries, err := os.ReadDir(c.Dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !keep[e.Name()] && !e.IsDir() {
			if err = os.Remove(filepath.Join(c.Dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
