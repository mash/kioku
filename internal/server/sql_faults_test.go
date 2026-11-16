package server

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

var fixtureDatabasePaths sync.Map

func fixtureSQL(t testing.TB, f *fixture, statement string) {
	t.Helper()
	path, ok := fixtureDatabasePaths.Load(f.stateStore)
	if !ok {
		t.Fatal("fixture database path missing")
	}
	db, err := sql.Open("sqlite", path.(string))
	fixtureCheck(t, err)
	defer db.Close()
	_, err = db.Exec(statement)
	fixtureCheck(t, err)
}
func fixtureSQLFault(t testing.TB, f *fixture, table, event, condition string) func() {
	t.Helper()
	fixtureSQL(t, f, "CREATE TRIGGER fixture_failure BEFORE "+event+" ON "+table+" WHEN "+condition+" BEGIN SELECT RAISE(ABORT,'injected SQL failure'); END")
	var once sync.Once
	restore := func() { once.Do(func() { fixtureSQL(t, f, "DROP TRIGGER fixture_failure") }) }
	t.Cleanup(restore)
	return restore
}

func fixtureSQLDatabase(t testing.TB, f *fixture) *sql.DB {
	t.Helper()
	path, ok := fixtureDatabasePaths.Load(f.stateStore)
	if !ok {
		t.Fatal("fixture database path missing")
	}
	db, err := sql.Open("sqlite", path.(string))
	fixtureCheck(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// fixtureSnapshot copies the fixture catalog to dir/catalog.sqlite with VACUUM INTO.
func fixtureSnapshot(t testing.TB, f *fixture, dir string) {
	t.Helper()
	fixtureCheck(t, os.MkdirAll(dir, 0700))
	fixtureSQL(t, f, "VACUUM INTO '"+filepath.Join(dir, "catalog.sqlite")+"'")
}
