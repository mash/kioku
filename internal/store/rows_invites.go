package store

func (q *Queries) PutInvite(v InviteRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO invites(id,project_id,created_by,expires,used_by,token_hash,data) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id,created_by=excluded.created_by,expires=excluded.expires,used_by=excluded.used_by,token_hash=excluded.token_hash,data=excluded.data", v.ID, v.ProjectID, v.CreatedBy, v.Expires, v.UsedBy, v.TokenHash, raw)
	return err
}
func (q *Queries) listInvite(suffix string, args ...any) ([]InviteRow, error) {
	rows, err := q.rows("SELECT id,project_id,created_by,expires,used_by,token_hash,data FROM invites "+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InviteRow{}
	for rows.Next() {
		v, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
