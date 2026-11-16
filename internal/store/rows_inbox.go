package store

const inboxColumns = "id,project_id,source_url,remove,created,run_id,data"

func (q *Queries) Inbox(id string) (InboxRow, error) {
	return scanInbox(q.row("SELECT "+inboxColumns+" FROM inbox WHERE id=?", id))
}
func (q *Queries) DeleteInbox(id string) error {
	_, err := q.exec("DELETE FROM inbox WHERE id=?", id)
	return err
}
func (q *Queries) PutInbox(v InboxRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO inbox("+inboxColumns+") VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET project_id=excluded.project_id,source_url=excluded.source_url,remove=excluded.remove,created=excluded.created,run_id=excluded.run_id,data=excluded.data", v.ID, v.ProjectID, v.SourceURL, v.Remove, v.Created, v.RunID, raw)
	return err
}
func (q *Queries) listInbox(suffix string, args ...any) ([]InboxRow, error) {
	return listRows(q, "SELECT "+inboxColumns+" FROM inbox "+suffix, scanInbox, args...)
}
