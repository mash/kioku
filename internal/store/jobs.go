package store

import (
	"database/sql"
	"time"
)

func (q *Queries) ProjectJobs(project string) ([]JobRow, error) {
	return q.listJob("WHERE project_id=? ORDER BY created DESC,id DESC", project)
}
func (q *Queries) ReconcileJobs() ([]JobRow, error) {
	return q.listJob("WHERE phase IS NOT NULL AND (cleanup_complete=0 OR status='running') ORDER BY id")
}
func (q *Queries) GenerationJobs() ([]JobRow, error) {
	return q.listJob("WHERE status='running' AND generation_snapshot IS NOT NULL ORDER BY created,id")
}
func (q *Queries) JobsBySession(id string) ([]JobRow, error) {
	return q.listJob("WHERE session_id=? ORDER BY id", id)
}
func (q *Queries) NextQueued(maxSessions int) (JobRow, error) {
	var active int
	if err := q.row("SELECT count(*) FROM jobs WHERE status='running' AND generation_snapshot IS NULL").Scan(&active); err != nil {
		return JobRow{}, err
	}
	if active >= maxSessions {
		return JobRow{}, sql.ErrNoRows
	}
	rows, err := q.listJob(`INDEXED BY jobs_queue WHERE status='queued' AND (parent_id IS NOT NULL OR NOT EXISTS(SELECT 1 FROM jobs AS active WHERE active.project_id=jobs.project_id AND active.status='running' AND active.parent_id IS NULL)) ORDER BY created,id LIMIT 1`)
	if err != nil {
		return JobRow{}, err
	}
	if len(rows) == 0 {
		return JobRow{}, sql.ErrNoRows
	}
	return rows[0], nil
}

// CompactHistory is part of the state-changing transaction. Cleanup and
// unconsumed child results are protected independently from public history.
func (q *Queries) CompactHistory(project string, now *int64) error {
	eligible := `project_id=? AND status IN ('completed','failed','skipped') AND (phase IS NULL OR cleanup_complete=1) AND NOT EXISTS(SELECT 1 FROM jobs AS parent WHERE parent.child_id=jobs.id)`
	ids := `SELECT id FROM jobs WHERE ` + eligible + ` ORDER BY created DESC,id DESC LIMIT -1 OFFSET 256`
	if _, err := q.exec("UPDATE inbox SET run_id=NULL WHERE run_id IN ("+ids+")", project); err != nil {
		return err
	}
	if _, err := q.exec("DELETE FROM jobs WHERE id IN ("+ids+")", project); err != nil {
		return err
	}
	if _, err := q.exec("DELETE FROM audit WHERE id IN (SELECT id FROM audit WHERE project_id=? ORDER BY created DESC,id DESC LIMIT -1 OFFSET 256)", project); err != nil {
		return err
	}
	if now != nil {
		ids = `SELECT id FROM jobs WHERE ` + eligible + ` AND created<?`
		cutoff := *now - int64(30*24*time.Hour/time.Millisecond)
		if _, err := q.exec("UPDATE inbox SET run_id=NULL WHERE run_id IN ("+ids+")", project, cutoff); err != nil {
			return err
		}
		if _, err := q.exec("DELETE FROM jobs WHERE id IN ("+ids+")", project, cutoff); err != nil {
			return err
		}
		if _, err := q.exec("DELETE FROM audit WHERE project_id=? AND created<?", project, *now-int64(90*24*time.Hour/time.Millisecond)); err != nil {
			return err
		}
	}
	return nil
}
func (q *Queries) ProjectAudit(project string) ([]AuditRow, error) {
	return q.listAudit("WHERE project_id=? ORDER BY created,id", project)
}
func (q *Queries) ProjectUsage(project string) ([]UsageRow, error) {
	return q.listUsage("WHERE project_id=? ORDER BY recorded_at,attempt_id", project)
}
