package store

func (q *Queries) Job(key0 string) (JobRow, error) {
	return scanJob(q.row("SELECT id,project_id,status,created,kind,session_id,input_ready,cleanup_complete,phase,output_hash,generation_snapshot,child_id,parent_id,data FROM jobs WHERE id=?", key0))
}
func (q *Queries) DeleteJob(key0 string) error {
	_, err := q.exec("DELETE FROM jobs WHERE id=?", key0)
	return err
}
func (q *Queries) PutJob(v JobRow) error {
	if v.Data.Views == nil {
		v.Data.Views = []ViewResultData{}
	}
	if v.Data.Run != nil && v.Data.Run.InboxIDs == nil {
		v.Data.Run.InboxIDs = []string{}
	}
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO jobs(id,project_id,status,created,kind,session_id,input_ready,cleanup_complete,phase,output_hash,generation_snapshot,child_id,parent_id,data) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id,status=excluded.status,created=excluded.created,kind=excluded.kind,session_id=excluded.session_id,input_ready=excluded.input_ready,cleanup_complete=excluded.cleanup_complete,phase=excluded.phase,output_hash=excluded.output_hash,generation_snapshot=excluded.generation_snapshot,child_id=excluded.child_id,parent_id=excluded.parent_id,data=excluded.data", v.ID, v.ProjectID, v.Status, v.Created, v.Kind, v.SessionID, v.InputReady, v.CleanupComplete, v.Phase, v.OutputHash, v.GenerationSnapshot, v.ChildID, v.ParentID, raw)
	return err
}
func (q *Queries) listJob(suffix string, args ...any) ([]JobRow, error) {
	rows, err := q.rows("SELECT id,project_id,status,created,kind,session_id,input_ready,cleanup_complete,phase,output_hash,generation_snapshot,child_id,parent_id,data FROM jobs "+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobRow{}
	for rows.Next() {
		v, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
