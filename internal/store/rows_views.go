package store

const viewsColumns = "project_id,id,slug,created_at,bundle_hash,view_epoch,data"

func (q *Queries) View(project string, id string) (ViewRow, error) {
	return scanView(q.row("SELECT "+viewsColumns+" FROM views WHERE project_id=? AND id=?", project, id))
}
func (q *Queries) DeleteView(project string, id string) error {
	_, err := q.exec("DELETE FROM views WHERE project_id=? AND id=?", project, id)
	return err
}
func (q *Queries) PutView(v ViewRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO views("+viewsColumns+") VALUES(?,?,?,?,?,?,?) ON CONFLICT(project_id,id) DO UPDATE SET slug=excluded.slug,created_at=excluded.created_at,bundle_hash=excluded.bundle_hash,view_epoch=excluded.view_epoch,data=excluded.data", v.ProjectID, v.ID, v.Slug, v.CreatedAt, v.BundleHash, v.ViewEpoch, raw)
	return err
}
func (q *Queries) listView(suffix string, args ...any) ([]ViewRow, error) {
	return listRows(q, "SELECT "+viewsColumns+" FROM views "+suffix, scanView, args...)
}
