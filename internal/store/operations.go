package store

import (
	"encoding/json"
	"errors"
)

func (q *Queries) Healthy() error {
	var raw string
	if err := q.row("SELECT data FROM schema_state WHERE id=1").Scan(&raw); err != nil {
		return err
	}
	var s struct {
		Ready bool `json:"ready"`
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return err
	}
	if !s.Ready {
		return errors.New("migration incomplete")
	}
	return nil
}
func (q *Queries) Metrics() (projects, queued, running, failed int, err error) {
	err = q.row("SELECT (SELECT count(*) FROM projects),(SELECT count(*) FROM jobs WHERE status='queued'),(SELECT count(*) FROM jobs WHERE status='running'),(SELECT count(*) FROM jobs WHERE status='failed')").Scan(&projects, &queued, &running, &failed)
	return
}

func (q *Queries) ProjectCounts(pid string) (inbox, pending int, err error) {
	err = q.row("SELECT (SELECT count(*) FROM inbox WHERE project_id=?),(SELECT count(*) FROM jobs WHERE project_id=? AND parent_id IS NULL AND status IN ('queued','running'))", pid, pid).Scan(&inbox, &pending)
	return
}
