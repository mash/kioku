package store

func (q *Queries) User(key0 string) (UserRow, error) {
	return scanUser(q.row("SELECT id,email,data FROM users WHERE id=?", key0))
}
func (q *Queries) DeleteUser(key0 string) error {
	_, err := q.exec("DELETE FROM users WHERE id=?", key0)
	return err
}
func (q *Queries) PutUser(v UserRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO users(id,email,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET email=excluded.email,data=excluded.data", v.ID, v.Email, raw)
	return err
}
func (q *Queries) listUser(suffix string, args ...any) ([]UserRow, error) {
	rows, err := q.rows("SELECT id,email,data FROM users "+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserRow{}
	for rows.Next() {
		v, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
