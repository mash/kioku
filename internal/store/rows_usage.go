package store

func (q *Queries) Usage(attemptID string) (UsageRow, error) {
	return scanUsage(q.row("SELECT attempt_id,project_id,job_id,recorded_at,data FROM usage WHERE attempt_id=?", attemptID))
}
func (q *Queries) RecordUsage(v UsageRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO usage(attempt_id,project_id,job_id,recorded_at,data) VALUES(?,?,?,?,?) ON CONFLICT(attempt_id) DO NOTHING", v.AttemptID, v.ProjectID, v.JobID, v.RecordedAt, raw)
	return err
}
func (q *Queries) listUsage(suffix string, args ...any) ([]UsageRow, error) {
	rows, err := q.rows("SELECT attempt_id,project_id,job_id,recorded_at,data FROM usage "+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		v, err := scanUsage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
