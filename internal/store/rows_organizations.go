package store

const organizationsColumns = "id,slug,data"

func (q *Queries) Organization(id string) (OrganizationRow, error) {
	return scanOrganization(q.row("SELECT "+organizationsColumns+" FROM organizations WHERE id=?", id))
}
func (q *Queries) DeleteOrganization(id string) error {
	_, err := q.exec("DELETE FROM organizations WHERE id=?", id)
	return err
}
func (q *Queries) PutOrganization(v OrganizationRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO organizations("+organizationsColumns+") VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET slug=excluded.slug,data=excluded.data", v.ID, v.Slug, raw)
	return err
}
func (q *Queries) listOrganization(suffix string, args ...any) ([]OrganizationRow, error) {
	return listRows(q, "SELECT "+organizationsColumns+" FROM organizations "+suffix, scanOrganization, args...)
}
