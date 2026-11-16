package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogFilesArePrivateWhileOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	d, err := OpenDatabase(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Errorf("catalog%s permissions = %04o, want 0600", suffix, got)
		}
	}
}

func TestCatalogConnectionProtectsFilesBeforeReading(t *testing.T) {
	d := newCatalog(t)
	catalogWrite(t, d, func(q *Queries) error {
		return q.PutUser(UserRow{ID: "u", Email: "private@example.com"})
	})
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(d.path+suffix, 0644); err != nil {
			t.Fatal(err)
		}
	}
	probe, err := catalogConnection(d.path, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(d.path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Errorf("catalog%s permissions = %04o, want 0600", suffix, got)
		}
	}
	var email string
	if err := probe.QueryRow("SELECT email FROM users WHERE id='u'").Scan(&email); err != nil || email != "private@example.com" {
		t.Fatalf("catalog contents changed: %q, %v", email, err)
	}
}
