package store

func (q *Queries) OAuthClient(clientID string) (OAuthClientRow, error) {
	return scanOAuthClient(q.row("SELECT client_id,data FROM oauth_clients WHERE client_id=?", clientID))
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
