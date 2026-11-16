package store

func (q *Queries) UserSessions(user, kind string) ([]SessionRow, error) {
	if kind == "" {
		return q.listSession("WHERE user_id=? ORDER BY id", user)
	}
	return q.listSession("WHERE user_id=? AND kind=? ORDER BY id", user, kind)
}
func (q *Queries) ProjectSessions(project, kind string) ([]SessionRow, error) {
	return q.listSession("WHERE project_id=? AND kind=? ORDER BY user_id,id", project, kind)
}

func (q *Queries) RevokeUserSessions(user string) error {
	_, err := q.exec("DELETE FROM sessions WHERE user_id=?", user)
	return err
}

// ConsumeOAuthCode marks an authorization code used. The row stays so that a
// replayed code can revoke its token family.
func (q *Queries) ConsumeOAuthCode(id string, now int64) (bool, error) {
	res, err := q.exec("UPDATE sessions SET status='active' WHERE id=? AND kind='oauth_code' AND status='pending' AND expires>?", id, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (q *Queries) ProjectUserSessions(project, user, kind string) ([]SessionRow, error) {
	return q.listSession("WHERE project_id=? AND kind=? AND user_id=? ORDER BY id", project, kind, user)
}
