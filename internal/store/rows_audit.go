package store

const auditColumns = "id,project_id,created,data"

func (q *Queries) Audit(id string) (AuditRow, error) {
	return scanAudit(q.row("SELECT "+auditColumns+" FROM audit WHERE id=?", id))
}
func (q *Queries) PutAudit(v AuditRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO audit("+auditColumns+") VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id,created=excluded.created,data=excluded.data", v.ID, v.ProjectID, v.Created, raw)
	return err
}
func (q *Queries) listAudit(suffix string, args ...any) ([]AuditRow, error) {
	return listRows(q, "SELECT "+auditColumns+" FROM audit "+suffix, scanAudit, args...)
}
