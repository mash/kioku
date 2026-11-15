package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

// Backup copies the whole catalog from a dedicated, pinned read snapshot.
// It never reads project objects or the encryption key. On failure the partial
// destination is retained for inspection, without affecting the live writer.
func (d *Database) Backup(dest string) error { return d.backup(context.Background(), dest, nil) }
func (d *Database) backup(ctx context.Context, dest string, afterSnapshot func() error) error {
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		return errors.New("backup destination must not exist")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(dest, 0700); err != nil {
		return err
	}
	path := filepath.Join(dest, "catalog.sqlite")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	source, err := catalogConnection(d.path, false, true)
	if err != nil {
		return err
	}
	defer source.Close()
	conn, err := source.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	var ready string
	if err = conn.QueryRowContext(ctx, "SELECT data FROM schema_state WHERE id=1").Scan(&ready); err != nil {
		return err
	}
	var state struct {
		Ready bool `json:"ready"`
	}
	if err = json.Unmarshal([]byte(ready), &state); err != nil {
		return err
	}
	if !state.Ready {
		return errors.New("cannot back up an incomplete migration")
	}
	captured := time.Now().UTC()
	if afterSnapshot != nil {
		if err = afterSnapshot(); err != nil {
			return err
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	params := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(5000)", "synchronous(FULL)"}}
	uri.RawQuery = params.Encode()
	if err = conn.Raw(func(raw any) error {
		source, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite driver does not support backup")
		}
		backup, e := source.NewBackup(uri.String())
		if e != nil {
			return e
		}
		_, stepErr := backup.Step(-1)
		finishErr := backup.Finish()
		return errors.Join(stepErr, finishErr)
	}); err != nil {
		return err
	}
	// Flush the complete file before publishing its manifest.
	f, err = os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	f, err = os.Open(path)
	if err != nil {
		return err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, f)
	closeErr = f.Close()
	if err = errors.Join(copyErr, closeErr); err != nil {
		return err
	}

	manifest, err := json.Marshal(map[string]any{"schema": 3, "captured_at": captured, "content_included": false, "sqlite_sha256": hex.EncodeToString(digest.Sum(nil))})
	if err != nil {
		return err
	}
	return AtomicWrite(filepath.Join(dest, "backup.json"), manifest, 0600)
}
