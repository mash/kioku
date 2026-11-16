package store

func (q *Queries) ProjectInvites(project string) ([]InviteRow, error) {
	return q.listInvite("WHERE project_id=? ORDER BY id", project)
}
func (q *Queries) InviteByToken(hash string) (InviteRow, error) {
	return scanInvite(q.row("SELECT "+invitesColumns+" FROM invites WHERE token_hash=?", hash))
}
