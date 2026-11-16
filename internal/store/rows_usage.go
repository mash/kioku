package store

const usageColumns = "attempt_id,project_id,job_id,recorded_at,data"

func (q *Queries) Usage(attemptID string) (UsageRow, error) {
	return scanUsage(q.row("SELECT "+usageColumns+" FROM usage WHERE attempt_id=?", attemptID))
}
func (q *Queries) RecordUsage(v UsageRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO usage("+usageColumns+") VALUES(?,?,?,?,?) ON CONFLICT(attempt_id) DO NOTHING", v.AttemptID, v.ProjectID, v.JobID, v.RecordedAt, raw)
	return err
}
func (q *Queries) listUsage(suffix string, args ...any) ([]UsageRow, error) {
	return listRows(q, "SELECT "+usageColumns+" FROM usage "+suffix, scanUsage, args...)
}
