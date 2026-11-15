package store

func (q *Queries) Organization(id string) (OrganizationRow, error) {
	return scanOrganization(q.row("SELECT id,slug,data FROM organizations WHERE id=?", id))
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
	_, err = q.exec("INSERT INTO organizations(id,slug,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET slug=excluded.slug,data=excluded.data", v.ID, v.Slug, raw)
	return err
}
func (q *Queries) listOrganization(suffix string, args ...any) ([]OrganizationRow, error) {
	rows, err := q.rows("SELECT id,slug,data FROM organizations "+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrganizationRow{}
	for rows.Next() {
		v, err := scanOrganization(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
