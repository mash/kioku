package local

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLocalTraversalAndGarbageCollectionListing(t *testing.T) {
	l := Store{Dir: t.TempDir()}
	ctx := context.Background()
	if err := l.Put(ctx, "../escape", nil); err == nil {
		t.Fatal("traversal accepted")
	}
	if err := l.Put(ctx, "projects/p/source", []byte("evidence")); err != nil {
		t.Fatal(err)
	}
	keys, err := l.List(ctx, "projects/p/")
	if err != nil || len(keys) != 1 {
		t.Fatal(keys, err)
	}
	if err = l.Delete(ctx, keys[0]); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsInvalidObjectKeys(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	ctx := context.Background()
	for _, key := range []Key{"", ".", "..", "../escape", "/absolute", "a/../../escape", "a/./b", "a//b", "a\\b", "a\x00b"} {
		if err := s.Put(ctx, key, []byte("bad")); err == nil {
			t.Errorf("Put accepted %q", key)
		}
		if _, err := s.Get(ctx, key); err == nil {
			t.Errorf("Get accepted %q", key)
		}
		if err := s.Delete(ctx, key); err == nil {
			t.Errorf("Delete accepted %q", key)
		}
	}
	for _, prefix := range []string{"", ".", "../", "/", "a/../../"} {
		if _, err := s.List(ctx, prefix); err == nil {
			t.Errorf("List accepted %q", prefix)
		}
	}
}

func TestSymlinkCannotEscapeStorageRoot(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	outside := t.TempDir()
	ctx := context.Background()
	target := filepath.Join(outside, "secret")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, to := range map[string]string{"directory": outside, "file": target} {
		if err := os.Symlink(to, filepath.Join(s.Dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Get(ctx, "directory/secret"); err == nil {
		t.Fatal("read escaped via directory symlink")
	}
	if _, err := s.Get(ctx, "file"); err == nil {
		t.Fatal("read escaped via file symlink")
	}
	if err := s.Put(ctx, "directory/secret", []byte("changed")); err == nil {
		t.Fatal("write escaped via directory symlink")
	}
	if err := s.Delete(ctx, "directory/secret"); err == nil {
		t.Fatal("delete escaped via directory symlink")
	}
	keys, err := s.List(ctx, "directory/")
	if err != nil || len(keys) != 0 {
		t.Fatalf("listed symlink contents: %v, %v", keys, err)
	}
	// Atomic replacement and deletion operate on the link, not its target.
	if err := s.Put(ctx, "file", []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "directory"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "original" {
		t.Fatalf("outside target changed: %q %v", body, err)
	}
}

func TestPrivateAtomicFilesAndSpecialFileRejection(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "objects")}
	ctx := context.Background()
	for _, body := range []string{"first", "second"} {
		if err := s.Put(ctx, "project/page", []byte(body)); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "project/page")
		if err != nil || string(got) != body {
			t.Fatalf("read: %q %v", got, err)
		}
	}
	info, err := os.Stat(filepath.Join(s.Dir, "project/page"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file permissions: %v %v", info, err)
	}
	info, err = os.Stat(filepath.Join(s.Dir, "project"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory permissions: %v %v", info, err)
	}
	if _, err := s.Get(ctx, "project"); err == nil {
		t.Fatal("read accepted directory")
	}
	fifo := filepath.Join(s.Dir, "project/fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "project/fifo"); err == nil {
		t.Fatal("read accepted FIFO")
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "project/.tmp-incomplete"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	keys, err := s.List(ctx, "project/")
	if err != nil || len(keys) != 1 || keys[0] != "project/page" {
		t.Fatalf("unexpected listing: %v %v", keys, err)
	}
}

func TestConcurrentSymlinkReplacementStaysWithinRoot(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	outside := t.TempDir()
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(s.Dir, "safe"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "page")
	if err := os.WriteFile(target, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, dest := range []string{"safe", outside} {
				_ = os.Remove(filepath.Join(s.Dir, "link"))
				_ = os.Symlink(dest, filepath.Join(s.Dir, "link"))
			}
		}
	}()
	for i := 0; i < 100; i++ {
		_ = s.Put(ctx, "link/page", []byte("inside"))
		if b, err := s.Get(ctx, "link/page"); err == nil && string(b) == "outside" {
			t.Error("read escaped root")
		}
		_ = s.Delete(ctx, "link/page")
	}
	close(stop)
	<-done
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "outside" {
		t.Fatalf("outside target modified: %q %v", b, err)
	}
}
