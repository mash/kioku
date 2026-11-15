package store

func (q *Queries) UserByIdentity(provider, id string) (UserRow, error) {
	var uid string
	if err := q.row("SELECT user_id FROM user_identities WHERE provider=? AND identity_id=?", provider, id).Scan(&uid); err != nil {
		return UserRow{}, err
	}
	return q.User(uid)
}
func (q *Queries) UsersByEmail(email string) ([]UserRow, error) {
	return q.listUser("WHERE kioku_equal_fold(email,?) ORDER BY id", email)
}
func (q *Queries) OrganizationBySlug(slug string) (OrganizationRow, error) {
	return scanOrganization(q.row("SELECT id,slug,data FROM organizations WHERE slug=?", slug))
}
func (q *Queries) PutIdentity(user, provider, id string) error {
	_, err := q.exec("INSERT INTO user_identities VALUES(?,?,?) ON CONFLICT(user_id,provider) DO UPDATE SET identity_id=excluded.identity_id", user, provider, id)
	return err
}
func (q *Queries) Identities(user string) (map[string]string, error) {
	rows, err := q.rows("SELECT provider,identity_id FROM user_identities WHERE user_id=? ORDER BY provider", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, id string
		if err := rows.Scan(&p, &id); err != nil {
			return nil, err
		}
		out[p] = id
	}
	return out, rows.Err()
}
func (q *Queries) ProjectRole(project, user string) (string, error) {
	var role string
	err := q.row("SELECT role FROM project_members WHERE project_id=? AND user_id=?", project, user).Scan(&role)
	return role, err
}
func (q *Queries) OrganizationRole(org, user string) (string, error) {
	var role string
	err := q.row("SELECT role FROM organization_members WHERE org_id=? AND user_id=?", org, user).Scan(&role)
	return role, err
}
func (q *Queries) PutProjectMember(project, user, role string) error {
	_, err := q.exec("INSERT INTO project_members VALUES(?,?,?) ON CONFLICT(project_id,user_id) DO UPDATE SET role=excluded.role", project, user, role)
	return err
}
func (q *Queries) PutOrganizationMember(org, user, role string) error {
	_, err := q.exec("INSERT INTO organization_members VALUES(?,?,?) ON CONFLICT(org_id,user_id) DO UPDATE SET role=excluded.role", org, user, role)
	return err
}
func (q *Queries) ProjectMembers(project string) (map[string]string, error) {
	return q.members("SELECT user_id,role FROM project_members WHERE project_id=?", project)
}
func (q *Queries) OrganizationMembers(org string) (map[string]string, error) {
	return q.members("SELECT user_id,role FROM organization_members WHERE org_id=?", org)
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
