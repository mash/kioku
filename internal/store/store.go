// Package store keeps account/configuration data in SQLite and project content
// in independently recoverable object storage. State is a joined in-memory view.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	_ "modernc.org/sqlite"
)

type Store struct {
	mu       sync.Mutex
	dir      string
	state    model.State
	lock     *os.File
	catalog  *sql.DB
	logged   map[blob.Key]bool
	objects  Objects
	poisoned error
}

func openDB(path string, create bool) (*sql.DB, error) {
	if !create {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("existing SQLite is unavailable: %w", err)
		}
		header := make([]byte, 16)
		_, err = f.Read(header)
		f.Close()
		if err != nil || string(header) != "SQLite format 3\x00" {
			return nil, fmt.Errorf("invalid existing SQLite: %s", path)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{"mode": {"rw"}}
	if create {
		q.Set("mode", "rwc")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*sql.DB, error) { db.Close(); return nil, err }
	if !create {
		var version int
		if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			return fail(err)
		}
		if version != 1 && version != 2 {
			return fail(fmt.Errorf("unsupported SQLite schema %d: %s", version, path))
		}
		var check string
		if err = db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil {
			return fail(err)
		}
		if check != "ok" {
			return fail(fmt.Errorf("SQLite integrity check failed: %s", check))
		}
		var n int
		if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('records','pending')").Scan(&n); err != nil || n != 2 {
			return fail(fmt.Errorf("incomplete SQLite schema: %s", path))
		}
	}
	queries := []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA secure_delete=ON", "PRAGMA busy_timeout=5000"}
	if create {
		queries = append(queries, `CREATE TABLE IF NOT EXISTS records(kind TEXT NOT NULL,id TEXT NOT NULL,payload BLOB NOT NULL,PRIMARY KEY(kind,id))`, `CREATE TABLE IF NOT EXISTS pending(id INTEGER PRIMARY KEY CHECK(id=1),payload BLOB NOT NULL)`, `PRAGMA user_version=2`)
	}
	for _, q := range queries {
		if _, err = db.Exec(q); err != nil {
			return fail(err)
		}
	}
	if err = os.Chmod(path, 0600); err != nil {
		return fail(err)
	}
	return db, nil
}

func validID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}
func Open(dir string) (*Store, error) {
	return OpenWithObjects(dir, fileObjects{dir: filepath.Join(dir, "objects")})
}

// OpenWithObjects loads the current content from its project buckets, never from
// a configuration backup. Exactly one active server owns this catalog lock.
func OpenWithObjects(dir string, objects Objects) (*Store, error) {
	return openStore(dir, objects, false)
}

// OpenControl opens configuration only, without requiring content storage.
func OpenControl(dir string) (*Store, error) { return openStore(dir, nil, true) }

func openStore(dir string, objects Objects, controlOnly bool) (*Store, error) {
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
	s := &Store{dir: dir, state: model.NewState(), lock: f, objects: objects}
	failed := true
	defer func() {
		if failed {
			s.Close()
		}
	}()
	catalog := filepath.Join(dir, "catalog.sqlite")
	_, statErr := os.Stat(catalog)
	create := os.IsNotExist(statErr)
	if statErr != nil && !create {
		return nil, statErr
	}
	if create {
		legacy, _ := filepath.Glob(filepath.Join(dir, "projects", "*", "project.sqlite"))
		content, _ := filepath.Glob(filepath.Join(dir, "objects", "projects", "*", ".kioku", "state.json"))
		if len(legacy) > 0 || len(content) > 0 {
			return nil, errors.New("catalog.sqlite missing while project storage exists; restore account configuration")
		}
	}
	s.catalog, err = openDB(catalog, create)
	if err != nil {
		return nil, err
	}
	var version int
	if err = s.catalog.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if controlOnly && version != 2 {
		return nil, errors.New("migrate legacy storage with the server before configuration-only backup")
	}
	var legacyJournal *model.State
	var pending []byte
	err = s.catalog.QueryRow("SELECT payload FROM pending WHERE id=1").Scan(&pending)
	if err == nil {
		if controlOnly {
			return nil, errors.New("unfinished transaction; start server with its object storage to recover before backup")
		}
		if version == 1 {
			var st = model.NewState()
			if err = json.Unmarshal(pending, &st); err != nil {
				return nil, err
			}
			legacyJournal = &st
			// Finish the old redo protocol before migrating its databases.
			if err = s.applyLegacy(st); err != nil {
				return nil, err
			}
		} else if err = s.recoverIntent(pending); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err = readRecords(s.catalog, &s.state); err != nil {
		return nil, err
	}
	if legacyJournal != nil {
		s.state = *legacyJournal
	}
	if controlOnly {
		failed = false
		return s, nil
	}
	for id, p := range s.state.Projects {
		if !validID(id) || p.ID != id {
			return nil, errors.New("invalid project configuration")
		}
	}
	registerBuckets(s.objects, s.state)
	if version == 1 {
		for pid := range s.state.Projects {
			db, e := s.projectDB(pid, false)
			if e != nil {
				return nil, e
			}
			e = readRecords(db, &s.state)
			db.Close()
			if e != nil {
				return nil, e
			}
		}
	}
	old := filepath.Join(dir, "metadata.json")
	oldJSON := false
	if b, e := os.ReadFile(old); e == nil {
		if len(s.state.Users) == 0 && len(s.state.Projects) == 0 {
			if err = json.Unmarshal(b, &s.state); err != nil {
				return nil, err
			}
			oldJSON = true
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	for id, p := range s.state.Projects {
		if !validID(id) || p.ID != id {
			return nil, errors.New("invalid project configuration")
		}
		if p.StorageRoot != "" && p.StorageRoot != "projects/"+id+"/" {
			return nil, errors.New("unsupported project storage root")
		}
		if p.StorageRoot == "" {
			p.StorageRoot = "projects/" + id + "/"
		}
		if p.StorageBucket == "" {
			if router, ok := objects.(interface{ Bucket(string) string }); ok {
				p.StorageBucket = router.Bucket(id)
			}
		}
		s.state.Projects[id] = p
	}
	if version == 1 || oldJSON {
		if err = s.archiveLegacy(); err != nil {
			return nil, err
		}
		full := s.state
		s.state = model.NewState() // Force preservation of all legacy job/audit records.
		exportCtx, cancelExport := context.WithTimeout(context.Background(), time.Hour)
		err = s.archiveRecords(exportCtx, full)
		s.state = full
		if err == nil {
			for pid := range full.Projects {
				if err = s.flushIntake(exportCtx, contentFor(full, pid)); err != nil {
					break
				}
			}
		}
		cancelExport()
		if err != nil {
			return nil, err
		}
		compactContent(&s.state)
		for pid := range s.state.Projects {
			if err = s.saveContent(s.state, pid); err != nil {
				return nil, err
			}
		}
		if err = writeRecords(s.catalog, s.state, ""); err != nil {
			return nil, err
		}
		if _, err = s.catalog.Exec("PRAGMA user_version=2"); err != nil {
			return nil, err
		}
		if oldJSON {
			if err = os.Rename(old, old+".migrated"); err != nil {
				return nil, err
			}
		}
		if err = os.RemoveAll(filepath.Join(dir, "projects")); err != nil {
			return nil, err
		}
		if _, err = s.catalog.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return nil, err
		}
	} else {
		for pid, p := range s.state.Projects {
			if err = s.loadContent(pid); err != nil {
				if p.Deleting && missingObject(err) {
					continue
				}
				return nil, fmt.Errorf("project %s content unavailable: %w", pid, err)
			}
		}
	}
	failed = false
	return s, nil
}

func (s *Store) projectDB(pid string, create bool) (*sql.DB, error) {
	if !validID(pid) {
		return nil, errors.New("invalid project ID")
	}
	dir := filepath.Join(s.dir, "projects", pid)
	if create {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	return openDB(filepath.Join(dir, "project.sqlite"), create)
}
func (s *Store) Close() error {
	if s.catalog != nil {
		_ = s.catalog.Close()
	}
	if s.lock != nil {
		return s.lock.Close()
	}
	return nil
}
func clone(st model.State) (model.State, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return model.State{}, err
	}
	var out model.State
	err = json.Unmarshal(b, &out)
	return out, err
}
func (s *Store) View(fn func(model.State) error) error {
	s.mu.Lock()
	if s.poisoned != nil {
		err := s.poisoned
		s.mu.Unlock()
		return err
	}
	st, err := clone(s.state)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return fn(st)
}

func (s *Store) Update(fn func(*model.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return s.poisoned
	}
	st, err := clone(s.state)
	if err != nil {
		return err
	}
	if err = fn(&st); err != nil {
		return err
	}
	before, _ := json.Marshal(s.state)
	after, _ := json.Marshal(st)
	if string(before) == string(after) {
		return nil
	}
	if err = s.persist(st); err != nil {
		s.poisoned = fmt.Errorf("metadata write failed; restart to recover: %w", err)
		return s.poisoned
	}
	compactContent(&st)
	s.state = st
	return nil
}
func (s *Store) applyLegacy(st model.State) error {
	// Each project transaction is atomic. The catalog journal survives until all
	// project transactions and the catalog transaction have committed.
	for pid := range st.Projects {
		if _, exists := s.state.Projects[pid]; exists && projectFingerprint(st, pid) == projectFingerprint(s.state, pid) {
			continue
		}
		_, exists := s.state.Projects[pid]
		db, err := s.projectDB(pid, !exists)
		if err != nil {
			return err
		}
		err = writeRecords(db, st, pid)
		db.Close()
		if err != nil {
			return err
		}
	}
	// Remove deleted project databases before committing catalog membership.
	entries, readErr := os.ReadDir(filepath.Join(s.dir, "projects"))
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, ok := st.Projects[e.Name()]; !ok {
			if err := os.RemoveAll(filepath.Join(s.dir, "projects", e.Name())); err != nil {
				return err
			}
		}
	}
	return writeRecords(s.catalog, st, "", true)
}
func projectFingerprint(st model.State, pid string) string {
	data := struct {
		Uploads   map[string]model.Upload
		Sources   map[string]model.Source
		Revisions map[string]model.Revision
		Jobs      map[string]model.Job
		Audit     map[string]model.Audit
	}{map[string]model.Upload{}, map[string]model.Source{}, map[string]model.Revision{}, map[string]model.Job{}, map[string]model.Audit{}}
	for id, v := range st.Uploads {
		if v.ProjectID == pid {
			data.Uploads[id] = v
		}
	}
	for id, v := range st.Sources {
		if v.ProjectID == pid {
			data.Sources[id] = v
		}
	}
	for id, v := range st.Revisions {
		if v.ProjectID == pid {
			data.Revisions[id] = v
		}
	}
	for id, v := range st.Jobs {
		if v.ProjectID == pid {
			data.Jobs[id] = v
		}
	}
	for id, v := range st.Audit {
		if v.ProjectID == pid {
			data.Audit[id] = v
		}
	}
	b, _ := json.Marshal(data)
	return string(b)
}

// retiredKinds lists record kinds that earlier versions wrote to the catalog
// for Stripe billing. The usage_ledger table of those versions also stays.
const retiredKinds = "('periods','holds','outbox','alerts','stripe_events')"

// retiredRow is one catalog row of a retired kind, or one usage_ledger row
// when kind is empty. Backup copies these rows unchanged.
type retiredRow struct {
	kind, id string
	payload  []byte
}

func readRetired(db *sql.DB) ([]retiredRow, error) {
	var out []retiredRow
	rows, err := db.Query("SELECT kind,id,payload FROM records WHERE kind IN " + retiredKinds)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r retiredRow
		if err = rows.Scan(&r.kind, &r.id, &r.payload); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var ledger int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='usage_ledger'").Scan(&ledger); err != nil || ledger == 0 {
		return out, err
	}
	rows, err = db.Query("SELECT id,payload FROM usage_ledger")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r retiredRow
		if err = rows.Scan(&r.id, &r.payload); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func writeRetired(db *sql.DB, rows []retiredRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS usage_ledger(id TEXT PRIMARY KEY,payload BLOB NOT NULL)`); err != nil {
		return err
	}
	for _, r := range rows {
		if r.kind == "" {
			_, err = tx.Exec("INSERT OR IGNORE INTO usage_ledger(id,payload) VALUES(?,?)", r.id, r.payload)
		} else {
			_, err = tx.Exec("INSERT OR IGNORE INTO records(kind,id,payload) VALUES(?,?,?)", r.kind, r.id, r.payload)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func writeRecords(db *sql.DB, st model.State, pid string, legacy ...bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The catalog keeps rows of retired kinds. No current field reads them, and
	// a save must not delete data that an earlier version stored.
	if _, err = tx.Exec("DELETE FROM records WHERE kind NOT IN " + retiredKinds); err != nil {
		return err
	}
	add := func(kind, id string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO records(kind,id,payload) VALUES(?,?,?)", kind, id, b)
		return err
	}
	if pid == "" {
		groups := []struct {
			kind  string
			value any
		}{{"oauth_clients", st.OAuthClients}, {"oauth_codes", st.OAuthCodes}, {"oauth_tokens", st.OAuthTokens}, {"login_codes", st.LoginCodes}, {"agent_tokens", st.AgentTokens}, {"users", st.Users}, {"projects", st.Projects}, {"invites", st.Invites}, {"sessions", st.Sessions}, {"oauth", st.OAuth}, {"devices", st.Devices}, {"organizations", st.Organizations}}
		for _, g := range groups {
			value := g.value
			if g.kind == "projects" && len(legacy) == 0 {
				value = projectConfigs(st)
			}
			b, _ := json.Marshal(value)
			var items map[string]json.RawMessage
			_ = json.Unmarshal(b, &items)
			for id, v := range items {
				if err = add(g.kind, id, v); err != nil {
					return err
				}
			}
		}
		if _, err = tx.Exec("DELETE FROM pending"); err != nil {
			return err
		}
	} else {
		for id, v := range st.Uploads {
			if v.ProjectID == pid {
				if err = add("uploads", id, v); err != nil {
					return err
				}
			}
		}
		for id, v := range st.Sources {
			if v.ProjectID == pid {
				if err = add("sources", id, v); err != nil {
					return err
				}
			}
		}
		for id, v := range st.Revisions {
			if v.ProjectID == pid {
				if err = add("revisions", id, v); err != nil {
					return err
				}
			}
		}
		for id, v := range st.Jobs {
			if v.ProjectID == pid {
				if err = add("jobs", id, v); err != nil {
					return err
				}
			}
		}
		for id, v := range st.Audit {
			if v.ProjectID == pid {
				if err = add("audit", id, v); err != nil {
					return err
				}
			}
		}
	}
	if pid == "" && len(legacy) == 0 {
		if _, err = tx.Exec("PRAGMA user_version=2"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func readRecords(db *sql.DB, st *model.State) error {
	rows, err := db.Query("SELECT kind,id,payload FROM records")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id string
		var b []byte
		if err = rows.Scan(&kind, &id, &b); err != nil {
			return err
		}
		wrapped, _ := json.Marshal(map[string]map[string]json.RawMessage{kind: {id: json.RawMessage(b)}})
		if err = json.Unmarshal(wrapped, st); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Backup exports only account and configuration data. Project content stays
// in object storage. This can run online without holding the store mutex over IO.
func (s *Store) Backup(dest string) error {
	s.mu.Lock()
	if s.poisoned != nil {
		err := s.poisoned
		s.mu.Unlock()
		return err
	}
	capturedAt := time.Now().UTC()
	b, err := controlJSON(s.state)
	var retired []retiredRow
	if err == nil {
		retired, err = readRetired(s.catalog)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err = os.Lstat(dest); !os.IsNotExist(err) {
		return errors.New("backup destination must not exist")
	}
	if err = os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	db, err := openDB(filepath.Join(dest, "catalog.sqlite"), true)
	if err != nil {
		return err
	}
	defer db.Close()
	st := model.NewState()
	if err = json.Unmarshal(b, &st); err != nil {
		return err
	}
	if err = writeRecords(db, st, ""); err != nil {
		return err
	}
	if err = writeRetired(db, retired); err != nil {
		return err
	}
	if err = db.Close(); err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(dest, "catalog.sqlite"))
	if err != nil {
		return err
	}
	digest := sha256.New()
	_, err = io.Copy(digest, file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	manifest, _ := json.Marshal(map[string]any{"schema": 2, "captured_at": capturedAt, "content_included": false, "sqlite_sha256": hex.EncodeToString(digest.Sum(nil))})
	return AtomicWrite(filepath.Join(dest, "backup.json"), manifest, 0600)
}
func AtomicWrite(path string, b []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
