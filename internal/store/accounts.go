package store

import (
	"database/sql"
	"errors"
)

// UserByIdentity returns sql.ErrNoRows only when the identity is unregistered.
func (q *Queries) UserByIdentity(provider, id string) (UserRow, error) {
	var uid string
	if err := q.row("SELECT user_id FROM user_identities WHERE provider=? AND identity_id=?", provider, id).Scan(&uid); err != nil {
		return UserRow{}, err
	}
	user, err := q.User(uid)
	if errors.Is(err, sql.ErrNoRows) {
		return UserRow{}, errors.New("registered identity has no user")
	}
	return user, err
}
func (q *Queries) UsersByEmail(email string) ([]UserRow, error) {
	return q.listUser("WHERE kioku_equal_fold(email,?) ORDER BY id", email)
}
func (q *Queries) OrganizationBySlug(slug string) (OrganizationRow, error) {
	return scanOrganization(q.row("SELECT "+organizationsColumns+" FROM organizations WHERE slug=?", slug))
}
func (q *Queries) PutIdentity(user, provider, id string) error {
	_, err := q.exec("INSERT INTO user_identities VALUES(?,?,?) ON CONFLICT(user_id,provider) DO UPDATE SET identity_id=excluded.identity_id", user, provider, id)
	return err
}
func (q *Queries) Identities(user string) (map[string]string, error) {
	return q.members("SELECT provider,identity_id FROM user_identities WHERE user_id=? ORDER BY provider", user)
}
func (q *Queries) ProjectRole(project, user string) (string, error) {
	var role string
	err := q.row("SELECT role FROM project_members WHERE project_id=? AND user_id=?", project, user).Scan(&role)
	return role, err
}
func (q *Queries) PutProjectMember(project, user, role string) error {
	_, err := q.exec("INSERT INTO project_members VALUES(?,?,?) ON CONFLICT(project_id,user_id) DO UPDATE SET role=excluded.role", project, user, role)
	return err
}
func (q *Queries) PutOrganizationOwner(org, user string) error {
	_, err := q.exec("INSERT INTO organization_owners VALUES(?,?) ON CONFLICT DO NOTHING", org, user)
	return err
}

func (q *Queries) DeleteOrganizationOwner(org, user string) error {
	_, err := q.exec("DELETE FROM organization_owners WHERE org_id=? AND user_id=?", org, user)
	return err
}
func (q *Queries) ProjectMembers(project string) (map[string]string, error) {
	return q.members("SELECT user_id,role FROM project_members WHERE project_id=?", project)
}
func (q *Queries) OrganizationOwners(org string) (map[string]bool, error) {
	return q.idSet("SELECT user_id FROM organization_owners WHERE org_id=?", org)
}
func (q *Queries) OwnedOrganizations(user string) (map[string]bool, error) {
	return q.idSet("SELECT org_id FROM organization_owners WHERE user_id=?", user)
}
func (q *Queries) idSet(query, id string) (map[string]bool, error) {
	rows, err := q.rows(query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}
func (q *Queries) members(query, id string) (map[string]string, error) {
	rows, err := q.rows(query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var user, role string
		if err := rows.Scan(&user, &role); err != nil {
			return nil, err
		}
		out[user] = role
	}
	return out, rows.Err()
}

func (q *Queries) OrganizationsForUser(user string) ([]OrganizationRow, error) {
	return q.listOrganization("WHERE id IN (SELECT org_id FROM organization_owners WHERE user_id=? UNION SELECT p.org_id FROM projects p JOIN project_members m ON m.project_id=p.id WHERE m.user_id=?) ORDER BY slug", user, user)
}
func (q *Queries) DeleteProjectMember(project, user string) error {
	_, err := q.exec("DELETE FROM project_members WHERE project_id=? AND user_id=?", project, user)
	return err
}
func (q *Queries) DeleteIdentity(user, provider string) error {
	_, err := q.exec("DELETE FROM user_identities WHERE user_id=? AND provider=?", user, provider)
	return err
}
func (q *Queries) DeleteAccountRows(user string) error {
	for _, table := range []string{"user_identities", "organization_owners", "project_members", "sessions"} {
		if _, err := q.exec("DELETE FROM "+table+" WHERE user_id=?", user); err != nil {
			return err
		}
	}
	if _, err := q.exec("DELETE FROM invites WHERE created_by=? AND used_by IS NULL", user); err != nil {
		return err
	}
	return q.DeleteUser(user)
}

func (q *Queries) OrganizationProjects(org string) ([]ProjectRow, error) {
	return q.listProject("WHERE org_id=? ORDER BY id", org)
}
func (q *Queries) DeleteOrganizationRows(org string) error {
	if _, err := q.exec("DELETE FROM organization_owners WHERE org_id=?", org); err != nil {
		return err
	}
	return q.DeleteOrganization(org)
}

func (q *Queries) OrganizationsByIDs(ids []string) ([]OrganizationRow, error) {
	raw, err := marshalData(ids)
	if err != nil {
		return nil, err
	}
	return q.listOrganization("WHERE id IN (SELECT value FROM json_each(?))", raw)
}
func (q *Queries) MembersForProjects(ids []string) (map[string]map[string]string, error) {
	raw, err := marshalData(ids)
	if err != nil {
		return nil, err
	}
	type membership struct{ Project, User, Role string }
	rows, err := listRows(q, "SELECT project_id,user_id,role FROM project_members WHERE project_id IN (SELECT value FROM json_each(?))", func(r rowScanner) (membership, error) {
		var m membership
		err := r.Scan(&m.Project, &m.User, &m.Role)
		return m, err
	}, raw)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for _, id := range ids {
		out[id] = map[string]string{}
	}
	for _, m := range rows {
		out[m.Project][m.User] = m.Role
	}
	return out, nil
}

func (q *Queries) OrganizationSlugTaken(slug string, now int64) (bool, error) {
	var taken bool
	err := q.row(`SELECT EXISTS(SELECT 1 FROM organizations WHERE slug=?
 UNION ALL SELECT 1 FROM user_slug_reservations WHERE slug=? AND expires>?)`, slug, slug, now).Scan(&taken)
	return taken, err
}

func (q *Queries) ReserveUserSlug(slug string, expires int64) error {
	_, err := q.exec("INSERT INTO user_slug_reservations(slug,expires) VALUES(?,?) ON CONFLICT(slug) DO UPDATE SET expires=excluded.expires", slug, expires)
	return err
}

func (q *Queries) OrganizationHasGeneral(org string) (bool, error) {
	var general bool
	err := q.row("SELECT EXISTS(SELECT 1 FROM projects p JOIN users u ON u.general_project_id=p.id WHERE p.org_id=?)", org).Scan(&general)
	return general, err
}
