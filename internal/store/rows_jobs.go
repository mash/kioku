package store

const jobsColumns = "id,project_id,status,created,kind,session_id,cleanup_complete,phase,output_hash,generation_snapshot,parent_id,data"

func (q *Queries) Job(id string) (JobRow, error) {
	return scanJob(q.row("SELECT "+jobsColumns+" FROM jobs WHERE id=?", id))
}

func (q *Queries) PutJob(v JobRow) error {
	if v.Data.Views == nil {
		v.Data.Views = []ViewResultData{}
	}
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO jobs("+jobsColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id,status=excluded.status,created=excluded.created,kind=excluded.kind,session_id=excluded.session_id,cleanup_complete=excluded.cleanup_complete,phase=excluded.phase,output_hash=excluded.output_hash,generation_snapshot=excluded.generation_snapshot,parent_id=excluded.parent_id,data=excluded.data", v.ID, v.ProjectID, v.Status, v.Created, v.Kind, v.SessionID, v.CleanupComplete, v.Phase, v.OutputHash, v.GenerationSnapshot, v.ParentID, raw)
	return err
}
func (q *Queries) listJob(suffix string, args ...any) ([]JobRow, error) {
	return listRows(q, "SELECT "+jobsColumns+" FROM jobs "+suffix, scanJob, args...)
}
