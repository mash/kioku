package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
)

// OpenStorage takes ownership of the process lock before loading a key or
// examining/migrating the catalog. loadKey must never create a replacement key
// when create is false. No server accepts work until this function succeeds.
func OpenStorage(ctx context.Context, dir string, objects blob.Store, loadKey func(create bool) (*auth.Cipher, error)) (*Database, *auth.Cipher, error) {
	lock, err := acquireCatalogLock(dir)
	if err != nil {
		return nil, nil, err
	}
	owned := false
	defer func() {
		if !owned {
			lock.Close()
		}
	}()
	path := filepath.Join(dir, "catalog.sqlite")
	_, statErr := os.Stat(path)
	create := os.IsNotExist(statErr)
	if statErr != nil && !create {
		return nil, nil, statErr
	}
	if create {
		if err = rejectOrphanLocalStorage(dir); err != nil {
			return nil, nil, err
		}
		if objects == nil {
			return nil, nil, errors.New("new catalog requires object storage verification")
		}
		keys, e := objects.List(ctx, "projects/")
		if e != nil {
			return nil, nil, e
		}
		if len(keys) != 0 {
			return nil, nil, errors.New("catalog.sqlite missing while project storage exists; restore verified backup")
		}
	} else if err = validateCatalogHeader(path); err != nil {
		return nil, nil, err
	}
	cipher, err := loadKey(create)
	if err != nil {
		return nil, nil, err
	}
	if cipher == nil {
		return nil, nil, errors.New("encryption key is required")
	}
	if !create {
		probe, e := catalogConnection(path, false, true)
		if e != nil {
			return nil, nil, e
		}
		var version int
		e = probe.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
		probe.Close()
		if e != nil {
			return nil, nil, e
		}
		switch version {
		case 2:
			if e = migrateV2(ctx, path, objects, cipher, time.Now().UTC()); e != nil {
				return nil, nil, e
			}
		case 3:
		default:
			return nil, nil, fmt.Errorf("unsupported SQLite schema %d", version)
		}
	}
	db, err := OpenDatabase(path, create)
	if err != nil {
		return nil, nil, err
	}
	if registry, ok := objects.(interface{ RegisterProjectBucket(string, string) }); ok {
		var projects []ProjectRow
		if err = db.Read(ctx, func(q *Queries) error {
			var err error
			projects, err = q.listProject("ORDER BY id")
			return err
		}); err != nil {
			db.Close()
			return nil, nil, err
		}
		for _, p := range projects {
			if p.Data.StorageBucket != nil {
				registry.RegisterProjectBucket(p.ID, *p.Data.StorageBucket)
			}
		}
	}
	db.lock = lock
	owned = true
	return db, cipher, nil
}
func OpenBackupDatabase(dir string) (*Database, error) {
	lock, err := acquireCatalogLock(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "catalog.sqlite")
	if err = validateCatalogHeader(path); err != nil {
		lock.Close()
		return nil, err
	}
	db, err := OpenDatabase(path, false)
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.lock = lock
	return db, nil
}
func acquireCatalogLock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "metadata.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("metadata is already open by another server")
	}
	return f, nil
}
func validateCatalogHeader(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	header := make([]byte, 16)
	if _, err = io.ReadFull(file, header); err != nil || string(header) != "SQLite format 3\x00" {
		return errors.New("invalid existing SQLite catalog")
	}
	return nil
}
func rejectOrphanLocalStorage(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".sqlite") || strings.HasPrefix(name, "catalog.sqlite-") || name == "metadata.json" || name == "state.json" {
			return errors.New("catalog.sqlite missing while legacy local storage exists")
		}
	}
	for _, name := range []string{"projects", "objects"} {
		err = filepath.WalkDir(filepath.Join(dir, name), func(_ string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				return errors.New("catalog.sqlite missing while legacy local storage exists")
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
