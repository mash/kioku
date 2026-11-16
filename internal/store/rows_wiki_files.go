package store

const wikiFileColumns = "project_id,path,hash,data"

func (q *Queries) WikiFile(project string, path string) (WikiFileRow, error) {
	return scanWikiFile(q.row("SELECT "+wikiFileColumns+" FROM wiki_files WHERE project_id=? AND path=?", project, path))
}
func (q *Queries) DeleteWikiFile(project string, path string) error {
	_, err := q.exec("DELETE FROM wiki_files WHERE project_id=? AND path=?", project, path)
	return err
}
func (q *Queries) PutWikiFile(v WikiFileRow) error {
	raw, err := marshalData(v.Data)
	if err != nil {
		return err
	}
	_, err = q.exec("INSERT INTO wiki_files("+wikiFileColumns+") VALUES(?,?,?,?) ON CONFLICT(project_id,path) DO UPDATE SET hash=excluded.hash,data=excluded.data", v.ProjectID, v.Path, v.Hash, raw)
	return err
}
func (q *Queries) listWikiFile(suffix string, args ...any) ([]WikiFileRow, error) {
	return listRows(q, "SELECT "+wikiFileColumns+" FROM wiki_files "+suffix, scanWikiFile, args...)
}
