package store

func (q *Queries) WikiFile(key0 string, key1 string) (WikiFileRow, error) {
	return scanWikiFile(q.row("SELECT project_id,path,hash,data FROM wiki_files WHERE project_id=? AND path=?", key0, key1))
}
func (q *Queries) DeleteWikiFile(key0 string, key1 string) error {
	_, err := q.exec("DELETE FROM wiki_files WHERE project_id=? AND path=?", key0, key1)
	return err
}
func (q *Queries) PutWikiFile(v WikiFileRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO wiki_files(project_id,path,hash,data) VALUES(?,?,?,?) ON CONFLICT(project_id,path) DO UPDATE SET hash=excluded.hash,data=excluded.data", v.ProjectID, v.Path, v.Hash, raw)
	return err
}
func (q *Queries) listWikiFile(suffix string, args ...any) ([]WikiFileRow, error) {
	rows, err := q.rows("SELECT project_id,path,hash,data FROM wiki_files "+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WikiFileRow{}
	for rows.Next() {
		v, err := scanWikiFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
