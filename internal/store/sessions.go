package store

import "errors"

func (q *Queries) UserSessions(user, kind string) ([]SessionRow, error) {
	if kind == "" {
		return q.listSession("WHERE user_id=? ORDER BY id", user)
	}
	return q.listSession("WHERE user_id=? AND kind=? ORDER BY id", user, kind)
}
func (q *Queries) ProjectSessions(project, kind string) ([]SessionRow, error) {
	return q.listSession("WHERE project_id=? AND kind=? ORDER BY user_id,id", project, kind)
}
func (q *Queries) KindSessions(kind string) ([]SessionRow, error) {
	return q.listSession("WHERE kind=? ORDER BY status,id", kind)
}
func (q *Queries) RevokeUserSessions(user string) error {
	_, err := q.exec("DELETE FROM sessions WHERE user_id=?", user)
	return err
}
func (q *Queries) ConsumeSession(id, kind string, now int64) (bool, error) {
	if kind != "cli_login" && kind != "oauth_code" && kind != "login_code" {
		return false, errors.New("session kind is not exchangeable")
	}
	res, err := q.exec("UPDATE sessions SET status='active' WHERE id=? AND kind=? AND status='pending' AND expires>?", id, kind, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
