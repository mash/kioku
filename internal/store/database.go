package store

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

// Database owns one writer and a small pool of independent, read-only snapshot connections.
// No transaction is retained across object-store or provider operations.
type Database struct {
	lock   *os.File
	writer *sql.DB
	reader *sql.DB
	path   string
	commit func(*sql.Tx) error // injectable commit response boundary for crash tests
}

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("kioku_equal_fold", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		a, aok := args[0].(string)
		b, bok := args[1].(string)
		if aok && bok && strings.EqualFold(a, b) {
			return int64(1), nil
		}
		return int64(0), nil
	})
}
func catalogConnection(path string, create, readOnly bool) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// SQLite inherits the database mode for WAL/SHM files. Protect the file
	// before opening any connection, including startup probes and migration.
	if create {
		f, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
	}
	if err = os.Chmod(abs, 0600); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err = os.Chmod(abs+suffix, 0600); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(5000)", "synchronous(FULL)", "secure_delete(ON)"}}
	if readOnly {
		q.Add("_pragma", "query_only(ON)")
	} else {
		q.Set("_txlock", "immediate")
		q.Add("_pragma", "journal_mode(WAL)")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	connections := 1
	if readOnly {
		connections = 4
	}
	db.SetMaxOpenConns(connections)
	db.SetMaxIdleConns(connections)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenDatabase opens a v3 catalog. Startup/migration owns the process lock and
// decides whether creation is permitted before calling this constructor.
func OpenDatabase(path string, create bool) (*Database, error) {
	w, err := catalogConnection(path, create, false)
	if err != nil {
		return nil, err
	}
	d := &Database{writer: w, path: path}
	fail := func(err error) (*Database, error) { d.Close(); return nil, err }
	if create {
		err = func() error {
			tx, err := w.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err = tx.Exec(catalogSchema); err != nil {
				return err
			}
			if _, err = tx.Exec(`INSERT INTO schema_state VALUES(1,'{"ready":true}'); PRAGMA user_version=3`); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			return fail(err)
		}
	}
	if err = d.validateCatalog(); err != nil {
		return fail(err)
	}
	d.reader, err = catalogConnection(path, false, true)
	if err != nil {
		return fail(err)
	}
	return d, nil
}
func (d *Database) Close() error {
	var errs []error
	if d.reader != nil {
		errs = append(errs, d.reader.Close())
	}
	if d.writer != nil {
		errs = append(errs, d.writer.Close())
	}
	if d.lock != nil {
		errs = append(errs, d.lock.Close())
		d.lock = nil
	}
	return errors.Join(errs...)
}
func (d *Database) validateCatalog() error {
	var version int
	var check, raw string
	if err := d.writer.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 3 {
		return fmt.Errorf("unsupported SQLite schema %d", version)
	}
	if err := d.writer.QueryRow("PRAGMA quick_check").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return errors.New("SQLite integrity check failed")
	}
	if err := d.writer.QueryRow("SELECT data FROM schema_state WHERE id=1").Scan(&raw); err != nil {
		return err
	}
	var state struct {
		Ready bool `json:"ready"`
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return err
	}
	if !state.Ready {
		return errors.New("migration incomplete; restore verified DB, objects and key")
	}
	// Compare sqlite_master definitions: this verifies column types, NOT NULL,
	// primary/unique keys, checks, and partial-index predicates together.
	rows, err := d.writer.Query("SELECT name,sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := map[string]string{}
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			return err
		}
		actual[name] = normalizeDDL(ddl)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, ddl := range strings.Split(catalogSchema, ";") {
		f := strings.Fields(ddl)
		if len(f) == 0 {
			continue
		}
		name := strings.Split(f[2], "(")[0]
		if actual[name] != normalizeDDL(ddl) {
			return fmt.Errorf("invalid catalog schema: %s", name)
		}
		delete(actual, name)
	}
	if len(actual) != 0 {
		return errors.New("unexpected catalog schema objects")
	}
	return nil
}
func normalizeDDL(s string) string { return strings.Join(strings.Fields(strings.TrimSpace(s)), " ") }
