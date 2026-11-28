package store

const usersColumns = "id,email,data,personal_org_id,general_project_id"

func (q *Queries) User(id string) (UserRow, error) {
	return scanUser(q.row("SELECT "+usersColumns+" FROM users WHERE id=?", id))
}
func (q *Queries) PersonalOrgUserID(orgID string) (string, error) {
	var id string
	err := q.row("SELECT id FROM users WHERE personal_org_id=?", orgID).Scan(&id)
	return id, err
}
func (q *Queries) DeleteUser(id string) error {
	_, err := q.exec("DELETE FROM users WHERE id=?", id)
	return err
}
func (q *Queries) PutUser(v UserRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO users("+usersColumns+") VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET email=excluded.email,data=excluded.data", v.ID, v.Email, raw, v.PersonalOrgID, v.GeneralProjectID)
	return err
}
func (q *Queries) listUser(suffix string, args ...any) ([]UserRow, error) {
	return listRows(q, "SELECT "+usersColumns+" FROM users "+suffix, scanUser, args...)
}
