package store

import (
	"errors"
	"fmt"
)

// catalogReady is the only readiness predicate for startup, health and backup.
func catalogReady(version int) error {
	if version == 3 {
		return nil
	}
	if version == 0 {
		return errors.New("migration incomplete; restore verified DB, objects and key")
	}
	return fmt.Errorf("unsupported SQLite schema %d", version)
}
func (q *Queries) Healthy() error {
	var version int
	if err := q.row("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	return catalogReady(version)
}
func (q *Queries) Metrics() (projects, queued, running, failed int, err error) {
	err = q.row("SELECT (SELECT count(*) FROM projects),(SELECT count(*) FROM jobs WHERE status='queued'),(SELECT count(*) FROM jobs WHERE status='running'),(SELECT count(*) FROM jobs WHERE status='failed')").Scan(&projects, &queued, &running, &failed)
	return
}

func (q *Queries) ProjectCounts(pid string) (inbox, pending int, err error) {
	err = q.row("SELECT (SELECT count(*) FROM inbox WHERE project_id=?),(SELECT count(*) FROM jobs WHERE project_id=? AND parent_id IS NULL AND status IN ('queued','running'))", pid, pid).Scan(&inbox, &pending)
	return
}
