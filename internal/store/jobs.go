package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

func (q *Queries) ProjectJobs(project string) ([]JobRow, error) {
	return q.listJob("WHERE project_id=? ORDER BY created DESC,id DESC", project)
}
func (q *Queries) RecentGenerationJobs(project string, limit int) ([]JobRow, error) {
	return q.listJob("WHERE project_id=? AND kind='generate' ORDER BY created DESC,id DESC LIMIT ?", project, limit)
}
func (q *Queries) ReconcileJobs() ([]JobRow, error) {
	return listRows(q, "SELECT "+jobsColumns+" FROM jobs WHERE phase IS NOT NULL AND cleanup_complete=0 UNION SELECT "+jobsColumns+" FROM jobs WHERE phase IS NOT NULL AND status='running' ORDER BY id", scanJob)
}
func (q *Queries) GenerationJobs() ([]JobRow, error) {
	return q.listJob("WHERE status='running' AND generation_snapshot IS NOT NULL ORDER BY created,id")
}
func (q *Queries) JobsBySession(id string) ([]JobRow, error) {
	return q.listJob("WHERE session_id=? ORDER BY id", id)
}
func (q *Queries) NextQueued(maxSessions int, deferred ...string) (JobRow, error) {
	var active int
	if err := q.row("SELECT count(*) FROM jobs WHERE status='running' AND generation_snapshot IS NULL").Scan(&active); err != nil {
		return JobRow{}, err
	}
	if active >= maxSessions {
		return JobRow{}, sql.ErrNoRows
	}
	exclusion := ""
	var args []any
	if len(deferred) != 0 {
		ids, err := json.Marshal(deferred)
		if err != nil {
			return JobRow{}, err
		}
		exclusion = " AND id NOT IN (SELECT value FROM json_each(?))"
		args = append(args, string(ids))
	}
	rows, err := q.listJob(`INDEXED BY jobs_queue WHERE status='queued' AND (parent_id IS NOT NULL OR NOT EXISTS(SELECT 1 FROM jobs AS active WHERE active.project_id=jobs.project_id AND active.status='running' AND active.parent_id IS NULL))`+exclusion+` ORDER BY created,id LIMIT 1`, args...)
	if err != nil {
		return JobRow{}, err
	}
	if len(rows) == 0 {
		return JobRow{}, sql.ErrNoRows
	}
	return rows[0], nil
}

// CompactHistory keeps the newest 256 finished jobs and audit rows of a project.
// With now, it also removes jobs older than 30 days and audit older than 90 days.
// Cleanup and unconsumed child results are protected independently from public history.
func (q *Queries) CompactHistory(project string, now *int64) error {
	eligible := `project_id=? AND status IN ('completed','failed','skipped') AND (phase IS NULL OR cleanup_complete=1) AND parent_id IS NULL`

	query := `SELECT id FROM (SELECT id FROM jobs WHERE ` + eligible + ` ORDER BY created DESC,id DESC LIMIT -1 OFFSET 256)`
	args := []any{project}
	if now != nil {
		query += ` UNION SELECT id FROM jobs WHERE ` + eligible + ` AND created<?`
		args = append(args, project, *now-int64(30*24*time.Hour/time.Millisecond))
	}
	ids, err := listRows(q, query, func(r rowScanner) (string, error) { var id string; err := r.Scan(&id); return id, err }, args...)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		raw, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		if _, err = q.exec("UPDATE inbox SET run_id=NULL WHERE project_id=? AND run_id IN (SELECT value FROM json_each(?))", project, string(raw)); err != nil {
			return err
		}
		if _, err = q.exec("DELETE FROM jobs WHERE id IN (SELECT value FROM json_each(?))", string(raw)); err != nil {
			return err
		}
	}
	if _, err = q.exec("DELETE FROM audit WHERE id IN (SELECT id FROM audit WHERE project_id=? ORDER BY created DESC,id DESC LIMIT -1 OFFSET 256)", project); err != nil {
		return err
	}
	if now != nil {
		if _, err = q.exec("DELETE FROM audit WHERE project_id=? AND created<?", project, *now-int64(90*24*time.Hour/time.Millisecond)); err != nil {
			return err
		}
	}
	return nil
}
func (q *Queries) ProjectAudit(project string) ([]AuditRow, error) {
	// Pruning runs hourly, so return only the newest 256 rows that it keeps.
	return q.listAudit("WHERE id IN (SELECT id FROM audit WHERE project_id=? ORDER BY created DESC,id DESC LIMIT 256) ORDER BY created,id", project)
}

func (q *Queries) UsageForJobs(ids []string) ([]UsageRow, error) {
	raw, err := marshalData(ids)
	if err != nil {
		return nil, err
	}
	return listRows(q, "SELECT "+usageColumns+" FROM usage WHERE job_id IN (SELECT value FROM json_each(?))", scanUsage, raw)
}

// GenerationChild finds the unconsumed child. Consumption clears parent_id and
// view_task in the same transaction as the parent's progress and budget.
func (q *Queries) GenerationChild(parent string) (JobRow, error) {
	return scanJob(q.row("SELECT "+jobsColumns+" FROM jobs WHERE parent_id=? AND status IN ('queued','running','completed','failed','skipped')", parent))
}
