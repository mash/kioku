package store

import "database/sql"

func (q *Queries) ProjectBySlug(org, slug string) (ProjectRow, error) {
	rows, err := q.listProject("WHERE org_id=? AND slug=?", org, slug)
	if err != nil {
		return ProjectRow{}, err
	}
	if len(rows) == 0 {
		return ProjectRow{}, sql.ErrNoRows
	}
	return rows[0], nil
}
func (q *Queries) ProjectsForUser(user string) ([]ProjectRow, error) {
	return q.listProject("WHERE id IN (SELECT project_id FROM project_members WHERE user_id=?) ORDER BY created,id", user)
}
func (q *Queries) DueProjects(now int64) ([]ProjectRow, error) {
	return q.listProject("WHERE next_generate_at IS NOT NULL AND deleting=0 AND next_generate_at<=? ORDER BY next_generate_at,id", now)
}
func (q *Queries) ProjectInbox(project string) ([]InboxRow, error) {
	return q.listInbox("WHERE project_id=? ORDER BY created,id", project)
}
func (q *Queries) AvailableInbox(project string, removeOnly bool) ([]InboxRow, error) {
	if removeOnly {
		return q.listInbox("WHERE project_id=? AND run_id IS NULL AND remove=1 ORDER BY created,id", project)
	}
	return q.listInbox("WHERE project_id=? AND run_id IS NULL ORDER BY created,id", project)
}
func (q *Queries) ProjectWiki(project string) ([]WikiFileRow, error) {
	return q.listWikiFile("WHERE project_id=? ORDER BY path", project)
}
func (q *Queries) ProjectViews(project string) ([]ViewRow, error) {
	return q.listView("WHERE project_id=? ORDER BY created_at IS NULL,created_at,CASE WHEN created_at IS NULL THEN slug ELSE id END", project)
}
func (q *Queries) ViewBySlug(project, slug string) (ViewRow, error) {
	rows, err := q.listView("WHERE project_id=? AND slug=?", project, slug)
	if err != nil {
		return ViewRow{}, err
	}
	if len(rows) == 0 {
		return ViewRow{}, sql.ErrNoRows
	}
	return rows[0], nil
}
func (q *Queries) ReplaceInbox(v InboxRow) error {
	if _, err := q.exec("DELETE FROM inbox WHERE project_id=? AND source_url=? AND run_id IS NULL", v.ProjectID, v.SourceURL); err != nil {
		return err
	}
	return q.PutInbox(v)
}
func (q *Queries) ReleaseInbox(project, run string) error {
	if _, err := q.exec(`DELETE FROM inbox WHERE project_id=? AND run_id=? AND EXISTS(SELECT 1 FROM inbox AS newer WHERE newer.project_id=inbox.project_id AND newer.source_url=inbox.source_url AND newer.run_id IS NULL)`, project, run); err != nil {
		return err
	}
	_, err := q.exec("UPDATE inbox SET run_id=NULL WHERE project_id=? AND run_id=?", project, run)
	return err
}
