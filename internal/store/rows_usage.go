package store

const usageColumns = "project_id,job_id,recorded_at,data"

func (q *Queries) Usage(jobID string) (UsageRow, error) {
	return scanUsage(q.row("SELECT "+usageColumns+" FROM usage WHERE job_id=?", jobID))
}
func (q *Queries) RecordUsage(v UsageRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO usage("+usageColumns+") VALUES(?,?,?,?) ON CONFLICT(job_id) DO NOTHING", v.ProjectID, v.JobID, v.RecordedAt, raw)
	return err
}
