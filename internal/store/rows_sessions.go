package store

func (q *Queries) Session(id string) (SessionRow, error) {
	return scanSession(q.row("SELECT id,kind,status,user_id,project_id,expires,created,data FROM sessions WHERE id=?", id))
}
func (q *Queries) DeleteSession(id string) error {
	_, err := q.exec("DELETE FROM sessions WHERE id=?", id)
	return err
}
func (q *Queries) PutSession(v SessionRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO sessions(id,kind,status,user_id,project_id,expires,created,data) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET kind=excluded.kind,status=excluded.status,user_id=excluded.user_id,project_id=excluded.project_id,expires=excluded.expires,created=excluded.created,data=excluded.data", v.ID, v.Kind, v.Status, v.UserID, v.ProjectID, v.Expires, v.Created, raw)
	return err
}
func (q *Queries) listSession(suffix string, args ...any) ([]SessionRow, error) {
	rows, err := q.rows("SELECT id,kind,status,user_id,project_id,expires,created,data FROM sessions "+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionRow{}
	for rows.Next() {
		v, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
