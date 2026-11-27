package store

const (
	generalOrganizationPrefix = "general-"
	GeneralProjectSlug        = "general"
)

func GeneralOrganizationID(user string) string { return generalOrganizationPrefix + user }

func IsGeneralProject(user, orgID, slug string) bool {
	return orgID == GeneralOrganizationID(user) && slug == GeneralProjectSlug
}

func (q *Queries) ProjectBySlug(org, slug string) (ProjectRow, error) {
	return scanProject(q.row("SELECT "+projectsColumns+" FROM projects WHERE org_id=? AND slug=?", org, slug))
}
func (q *Queries) GeneralProject(user string) (ProjectRow, error) {
	return q.ProjectBySlug(GeneralOrganizationID(user), GeneralProjectSlug)
}
func (q *Queries) UsersWithoutGeneral() ([]UserRow, error) {
	return q.listUser(`WHERE NOT EXISTS (
		SELECT 1 FROM projects WHERE org_id=? || users.id AND slug=?
	) ORDER BY id`, generalOrganizationPrefix, GeneralProjectSlug)
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
func (q *Queries) RunInbox(project, run string) ([]InboxRow, error) {
	return q.listInbox("WHERE project_id=? AND run_id=? ORDER BY created,id", project, run)
}
func (q *Queries) ProjectWiki(project string) ([]WikiFileRow, error) {
	return q.listWikiFile("WHERE project_id=? ORDER BY path", project)
}
func (q *Queries) ProjectViews(project string) ([]ViewRow, error) {
	return q.listView("WHERE project_id=? ORDER BY created_at IS NULL,created_at,CASE WHEN created_at IS NULL THEN slug ELSE id END", project)
}
func (q *Queries) ViewBySlug(project, slug string) (ViewRow, error) {
	return scanView(q.row("SELECT "+viewsColumns+" FROM views WHERE project_id=? AND slug=?", project, slug))
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

func (q *Queries) ClearProjectContent(project string) error {
	for _, table := range []string{"wiki_files", "views", "inbox"} {
		if _, err := q.exec("DELETE FROM "+table+" WHERE project_id=?", project); err != nil {
			return err
		}
	}
	return nil
}
func (q *Queries) DeleteProjectRows(project string) error {
	for _, table := range []string{"project_members", "inbox", "wiki_files", "views", "jobs", "audit", "invites"} {
		if _, err := q.exec("DELETE FROM "+table+" WHERE project_id=?", project); err != nil {
			return err
		}
	}
	return q.DeleteProject(project)
}

// PruneExpired removes expired sessions and invites, and compacts the job and
// audit history of every project. The hourly maintenance pass runs it.
func (q *Queries) PruneExpired(now int64) error {
	if err := q.PruneSessions(now); err != nil {
		return err
	}
	if _, err := q.exec("DELETE FROM invites WHERE expires<=?", now); err != nil {
		return err
	}
	ids, err := listRows(q, "SELECT id FROM projects", func(r rowScanner) (string, error) { var id string; err := r.Scan(&id); return id, err })
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := q.CompactHistory(id, &now); err != nil {
			return err
		}
	}
	return nil
}

func (q *Queries) InboxCountExceptReplacement(project, source string) (int, error) {
	var count int
	err := q.row("SELECT count(*) FROM inbox WHERE project_id=? AND (run_id IS NOT NULL OR source_url<>?)", project, source).Scan(&count)
	return count, err
}
func (q *Queries) HasAvailableInbox(project string, removeOnly bool) (bool, error) {
	query := "SELECT EXISTS(SELECT 1 FROM inbox WHERE project_id=? AND run_id IS NULL"
	if removeOnly {
		query += " AND remove=1"
	}
	var found bool
	err := q.row(query+")", project).Scan(&found)
	return found, err
}
func (q *Queries) HasPendingGeneration(project string) (bool, error) {
	var found bool
	err := q.row("SELECT EXISTS(SELECT 1 FROM jobs WHERE project_id=? AND status IN ('queued','running') AND parent_id IS NULL)", project).Scan(&found)
	return found, err
}

func (q *Queries) InboxForProjects(ids []string) ([]InboxRow, error) {
	raw, err := marshalData(ids)
	if err != nil {
		return nil, err
	}
	return q.listInbox("WHERE project_id IN (SELECT value FROM json_each(?)) ORDER BY created,id", raw)
}
