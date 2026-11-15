package store

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
)

func TestStartupRejectsMissingDBWithObjects(t *testing.T) {
	for _, which := range []string{"remote", "local", "list failure"} {
		t.Run(which, func(t *testing.T) {
			dir := t.TempDir()
			objects := &startupObjects{migrationObjects: migrationObjects{values: map[blob.Key][]byte{}}}
			switch which {
			case "remote":
				objects.values["projects/lost/.kioku/wiki/file.md"] = []byte("lost")
			case "local":
				if e := os.MkdirAll(filepath.Join(dir, "projects", "lost"), 0700); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(filepath.Join(dir, "projects", "lost", "project.sqlite"), []byte("lost"), 0600); e != nil {
					t.Fatal(e)
				}
			case "list failure":
				objects.fail = true
			}
			called := false
			_, _, e := OpenStorage(t.Context(), dir, objects, func(bool) (*auth.Cipher, error) { called = true; return nil, errors.New("unexpected key creation") })
			if e == nil || called {
				t.Fatal(e, called)
			}
			if _, e = os.Stat(filepath.Join(dir, "catalog.sqlite")); !os.IsNotExist(e) {
				t.Fatal(e)
			}
			if objects.puts != 0 || objects.deletes != 0 {
				t.Fatal(objects)
			}
		})
	}
}

type startupObjects struct {
	migrationObjects
	fail bool
}

func (m *startupObjects) List(ctx context.Context, p string) ([]blob.Key, error) {
	if m.fail {
		return nil, errors.New("LIST unavailable")
	}
	return m.migrationObjects.List(ctx, p)
}
func TestStartupHoldsLockBeforeKeyAndRefusesMissingExistingKey(t *testing.T) {
	dir := t.TempDir()
	objects := &startupObjects{migrationObjects: migrationObjects{values: map[blob.Key][]byte{}}}
	cipher, _ := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	calls := []bool{}
	loader := func(create bool) (*auth.Cipher, error) {
		calls = append(calls, create)
		if lock, e := acquireCatalogLock(dir); e == nil {
			lock.Close()
			t.Fatal("key loaded without catalog lock")
		}
		return cipher, nil
	}
	d, c, e := OpenStorage(t.Context(), dir, objects, loader)
	if e != nil || c != cipher {
		t.Fatal(e)
	}
	if _, _, e = OpenStorage(t.Context(), dir, objects, loader); e == nil {
		t.Fatal("opened locked catalog")
	}
	d.Close()
	d, _, e = OpenStorage(t.Context(), dir, objects, loader)
	if e != nil {
		t.Fatal(e)
	}
	d.Close()
	if !reflect.DeepEqual(calls, []bool{true, false}) {
		t.Fatal(calls)
	}
	_, _, e = OpenStorage(t.Context(), dir, objects, func(create bool) (*auth.Cipher, error) {
		if create {
			t.Fatal("attempted replacement key")
		}
		return nil, errors.New("existing key missing")
	})
	if e == nil {
		t.Fatal("accepted missing key")
	}
}
func TestStartupRejectsInvalidExistingSchemaWithoutGC(t *testing.T) {
	for _, body := range [][]byte{nil, []byte("not SQLite")} {
		dir := t.TempDir()
		if e := os.WriteFile(filepath.Join(dir, "catalog.sqlite"), body, 0600); e != nil {
			t.Fatal(e)
		}
		objects := &startupObjects{migrationObjects: migrationObjects{values: map[blob.Key][]byte{}}}
		if _, _, e := OpenStorage(t.Context(), dir, objects, func(bool) (*auth.Cipher, error) {
			return auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
		}); e == nil {
			t.Fatal("accepted corrupt database")
		}
		if objects.deletes != 0 {
			t.Fatal("ran GC")
		}
	}
}
