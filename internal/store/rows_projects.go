package store

func (q *Queries) Project(id string) (ProjectRow, error) {
	return scanProject(q.row("SELECT id,org_id,slug,created,deleting,wiki_revision,wiki_incomplete,next_generate_at,view_epoch,data FROM projects WHERE id=?", id))
}
func (q *Queries) DeleteProject(id string) error {
	_, err := q.exec("DELETE FROM projects WHERE id=?", id)
	return err
}
func (q *Queries) PutProject(v ProjectRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO projects(id,org_id,slug,created,deleting,wiki_revision,wiki_incomplete,next_generate_at,view_epoch,data) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET org_id=excluded.org_id,slug=excluded.slug,created=excluded.created,deleting=excluded.deleting,wiki_revision=excluded.wiki_revision,wiki_incomplete=excluded.wiki_incomplete,next_generate_at=excluded.next_generate_at,view_epoch=excluded.view_epoch,data=excluded.data", v.ID, v.OrgID, v.Slug, v.Created, v.Deleting, v.WikiRevision, v.WikiIncomplete, v.NextGenerateAt, v.ViewEpoch, raw)
	return err
}
func (q *Queries) listProject(suffix string, args ...any) ([]ProjectRow, error) {
	rows, err := q.rows("SELECT id,org_id,slug,created,deleting,wiki_revision,wiki_incomplete,next_generate_at,view_epoch,data FROM projects "+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectRow{}
	for rows.Next() {
		v, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
