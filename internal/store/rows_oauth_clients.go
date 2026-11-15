package store

func (q *Queries) OAuthClient(key0 string) (OAuthClientRow, error) {
	return scanOAuthClient(q.row("SELECT client_id,data FROM oauth_clients WHERE client_id=?", key0))
}
func (q *Queries) DeleteOAuthClient(key0 string) error {
	_, err := q.exec("DELETE FROM oauth_clients WHERE client_id=?", key0)
	return err
}
func (q *Queries) PutOAuthClient(v OAuthClientRow) error {
	if v.Data.RedirectURIs == nil {
		v.Data.RedirectURIs = []string{}
	}
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO oauth_clients(client_id,data) VALUES(?,?) ON CONFLICT(client_id) DO UPDATE SET data=excluded.data", v.ClientID, raw)
	return err
}
func (q *Queries) listOAuthClient(suffix string, args ...any) ([]OAuthClientRow, error) {
	rows, err := q.rows("SELECT client_id,data FROM oauth_clients "+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OAuthClientRow{}
	for rows.Next() {
		v, err := scanOAuthClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
