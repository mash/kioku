// Package local stores objects beneath an operator-controlled directory.
package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mash/kioku/internal/blob"
)

type Key = blob.Key
type Config struct{ DataDir string }
type Store struct{ Dir string }

func New(c Config) (Store, error) {
	if c.DataDir == "" {
		return Store{}, errors.New("local storage requires a data directory")
	}
	return Store{Dir: filepath.Join(c.DataDir, "objects")}, nil
}

// Dir is trusted deployment configuration. Once opened, all object operations
// are relative to this root, including during concurrent symlink replacement.
func (l Store) open(create bool) (*os.Root, error) {
	if l.Dir == "" {
		return nil, errors.New("local storage directory is empty")
	}
	if create {
		if err := os.MkdirAll(l.Dir, 0700); err != nil {
			return nil, err
		}
	}
	return os.OpenRoot(l.Dir)
}

func validKey(key Key) bool { return key != "." && blob.ValidKey(key) }

func (l Store) Get(ctx context.Context, key Key) ([]byte, error) {
	if !validKey(key) {
		return nil, errors.New("invalid object key")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := l.open(false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// Nonblocking open lets us reject FIFOs instead of hanging on them.
	f, err := root.OpenFile(string(key), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("object is not a regular file")
	}
	return io.ReadAll(f)
}

func (l Store) Put(ctx context.Context, key Key, body []byte) error {
	if !validKey(key) {
		return errors.New("invalid object key")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := l.open(true)
	if err != nil {
		return err
	}
	defer root.Close()
	dir := path.Dir(string(key))
	if err = root.MkdirAll(dir, 0700); err != nil {
		return err
	}
	parent, err := root.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	// Pin the destination directory and create a private, exclusive temporary
	// file. Rename replaces the directory entry; it never follows a target link.
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	tmp := ".tmp-" + hex.EncodeToString(random[:])
	f, err := parent.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer parent.Remove(tmp)
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = parent.Rename(tmp, path.Base(string(key))); err != nil {
		return err
	}
	d, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (l Store) Delete(ctx context.Context, key Key) error {
	if !validKey(key) {
		return errors.New("invalid object key")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := l.open(false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	// Remove unlinks a final symlink itself, without deleting its target.
	err = root.Remove(string(key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (l Store) List(ctx context.Context, prefix string) ([]Key, error) {
	if !validKey(Key(strings.TrimSuffix(prefix, "/"))) {
		return nil, errors.New("invalid prefix")
	}
	root, err := l.open(false)
	if os.IsNotExist(err) {
		return []Key{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	keys := []Key{}
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		// WalkDir never follows symlink directories. Exclude symlink files, special
		// files and incomplete atomic writes from the object listing as well.
		if d.IsDir() || !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		if strings.HasPrefix(name, prefix) {
			keys = append(keys, Key(name))
		}
		return nil
	})
	return keys, err
}

var _ blob.Store = Store{}
