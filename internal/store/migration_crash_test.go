package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/model"
)

type migrationCrashObjects struct {
	blob.Store
	point string
}

func (b migrationCrashObjects) Put(ctx context.Context, key blob.Key, body []byte) error {
	if err := b.Store.Put(ctx, key, body); err != nil {
		return err
	}
	if b.point == "put" {
		os.Exit(91)
	}
	return nil
}
func (b migrationCrashObjects) Delete(ctx context.Context, key blob.Key) error {
	if err := b.Store.Delete(ctx, key); err != nil {
		return err
	}
	if b.point == "delete" {
		os.Exit(91)
	}
	return nil
}
func TestMigrationCrashChild(t *testing.T) {
	dir := os.Getenv("KIOKU_MIGRATION_CRASH_DIR")
	if dir == "" {
		return
	}
	objects := migrationCrashObjects{Store: local.Store{Dir: os.Getenv("KIOKU_MIGRATION_CRASH_OBJECTS")}, point: os.Getenv("KIOKU_MIGRATION_CRASH_POINT")}
	_, _, err := OpenStorage(t.Context(), dir, objects, func(create bool) (*auth.Cipher, error) {
		if create {
			t.Fatal("migration replaced the existing key")
		}
		return auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	})
	t.Fatalf("crash point not reached: %v", err)
}
func TestMigrationFailureRequiresVerifiedRestore(t *testing.T) {
	for _, point := range []string{"put", "delete"} {
		t.Run(point, func(t *testing.T) {
			path, originals, cipher, content := migrationFixture(t)
			body := []byte("# Restored Wiki\n")
			content.Wiki = &legacyV2Revision{ID: "r", ProjectID: "p", StoredBytes: int64(len(body)), Created: time.UnixMilli(1000), Files: map[string]blob.Key{"wiki/index.md": "projects/p/wiki/index.md"}, FileHashes: map[string]string{"wiki/index.md": string(model.HashContent(body))}, FileBytes: map[string]int64{"wiki/index.md": int64(len(body))}}
			originals.set(t, "projects/p/.kioku/state.json", content)
			originals.values["projects/p/wiki/index.md"] = body
			oldDB, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			objects := local.Store{Dir: t.TempDir()}
			for key, b := range originals.values {
				if err := objects.Put(t.Context(), key, b); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMigrationCrashChild$")
			cmd.Env = append(os.Environ(), "KIOKU_MIGRATION_CRASH_DIR="+filepath.Dir(path), "KIOKU_MIGRATION_CRASH_OBJECTS="+objects.Dir, "KIOKU_MIGRATION_CRASH_POINT="+point)
			output, err := cmd.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 91 {
				t.Fatalf("child did not crash: %v\n%s", err, output)
			}
			if point == "delete" {
				db, _, err := OpenStorage(t.Context(), filepath.Dir(path), objects, func(bool) (*auth.Cipher, error) { return cipher, nil })
				if err == nil {
					db.Close()
					t.Fatal("incomplete user_version accepted after crash")
				}
				if !strings.Contains(err.Error(), "migration incomplete") {
					t.Fatal(err)
				}
			} else {
				// Before the schema commit the old state must remain intact. The
				// operator still restores the matched set before retrying migration.
				for key, want := range originals.values {
					got, err := objects.Get(t.Context(), key)
					if err != nil || !bytes.Equal(got, want) {
						t.Fatal("old object changed before commit", key, err)
					}
				}
			}
			restoredDir := t.TempDir()
			restoredObjects := local.Store{Dir: t.TempDir()}
			if err := os.WriteFile(filepath.Join(restoredDir, "catalog.sqlite"), oldDB, 0600); err != nil {
				t.Fatal(err)
			}
			for key, b := range originals.values {
				if err := restoredObjects.Put(t.Context(), key, b); err != nil {
					t.Fatal(err)
				}
			}
			db, _, err := OpenStorage(t.Context(), restoredDir, restoredObjects, func(create bool) (*auth.Cipher, error) {
				if create {
					t.Error("restoration created key")
				}
				return cipher, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.Read(t.Context(), func(q *Queries) error {
				f, err := q.WikiFile("p", "wiki/index.md")
				if f.Hash != string(model.HashContent(body)) || f.Data.Bytes != int64(len(body)) {
					t.Fatal(f)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			got, err := restoredObjects.Get(t.Context(), model.WikiContentKey("p", model.HashContent(body)))
			if err != nil || !bytes.Equal(got, body) {
				t.Fatal("restored body", err)
			}
		})
	}
}

type sqliteResultError int

func (e sqliteResultError) Error() string { return "injected SQLite result" }
func (e sqliteResultError) Code() int     { return int(e) }

// This test replaces exitProcess, so it must not run in parallel.
func TestSQLiteCorruptionStopsButConstraintFailureIsLocal(t *testing.T) {
	d := newCatalog(t)
	exited := false
	exitProcess = func(int) { exited = true }
	t.Cleanup(func() { exitProcess = os.Exit })
	for _, code := range []int{11, 11 | 256, 26, 10, 19, 5} {
		exited = false
		err := d.Write(t.Context(), func(q *Queries) error {
			if e := q.PutUser(UserRow{ID: "rollback", Email: "rollback"}); e != nil {
				return e
			}
			return sqliteResultError(code)
		})
		want := code&255 == 11 || code&255 == 26 || code&255 == 10
		if exited != want || err == nil {
			t.Fatalf("code=%d exited=%t error=%v", code, exited, err)
		}
		if err := d.Read(t.Context(), func(q *Queries) error {
			_, e := q.User("rollback")
			if !errors.Is(e, sql.ErrNoRows) {
				t.Fatal("failed mutation committed", e)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
