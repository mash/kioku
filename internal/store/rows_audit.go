package store

func (q *Queries) Audit(key0 string) (AuditRow, error) {
	return scanAudit(q.row("SELECT id,project_id,created,data FROM audit WHERE id=?", key0))
}
func (q *Queries) DeleteAudit(key0 string) error {
	_, err := q.exec("DELETE FROM audit WHERE id=?", key0)
	return err
}
func (q *Queries) PutAudit(v AuditRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO audit(id,project_id,created,data) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id,created=excluded.created,data=excluded.data", v.ID, v.ProjectID, v.Created, raw)
	return err
}
func (q *Queries) listAudit(suffix string, args ...any) ([]AuditRow, error) {
	rows, err := q.rows("SELECT id,project_id,created,data FROM audit "+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditRow{}
	for rows.Next() {
		v, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
