package store

const invitesColumns = "id,project_id,created_by,expires,used_by,token_hash,data"

func (q *Queries) PutInvite(v InviteRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO invites("+invitesColumns+") VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id,created_by=excluded.created_by,expires=excluded.expires,used_by=excluded.used_by,token_hash=excluded.token_hash,data=excluded.data", v.ID, v.ProjectID, v.CreatedBy, v.Expires, v.UsedBy, v.TokenHash, raw)
	return err
}
func (q *Queries) listInvite(suffix string, args ...any) ([]InviteRow, error) {
	return listRows(q, "SELECT "+invitesColumns+" FROM invites "+suffix, scanInvite, args...)
}
