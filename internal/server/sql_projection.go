package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func storageDate(v int64) time.Time { return time.UnixMilli(v).UTC() }
func optionalDate(v *int64) time.Time {
	if v == nil {
		return time.Time{}
	}
	return storageDate(*v)
}
func projectProjection(q *store.Queries, row store.ProjectRow) (model.Project, error) {
	members, err := q.ProjectMembers(row.ID)
	if err != nil {
		return model.Project{}, err
	}
	org, err := q.Organization(row.OrgID)
	if err != nil {
		return model.Project{}, err
	}
	return projectValue(row, org.Slug, members), nil
}
func projectValue(row store.ProjectRow, orgSlug string, members map[string]string) model.Project {
	return model.Project{ID: row.ID, OrgID: row.OrgID, Slug: row.Slug, Namespace: orgSlug + "/" + row.Slug, Name: row.Data.Name, Members: members, Created: storageDate(row.Created), Deleting: row.Deleting, Revision: store.ValueString(row.WikiRevision), WikiUpdatedAt: optionalDate(row.Data.WikiUpdatedAt)}
}
func projectProjections(q *store.Queries, rows []store.ProjectRow) ([]model.Project, error) {
	out := make([]model.Project, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids, orgIDs := make([]string, 0, len(rows)), make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
		orgIDs = append(orgIDs, row.OrgID)
	}
	members, err := q.MembersForProjects(ids)
	if err != nil {
		return nil, err
	}
	orgs, err := q.OrganizationsByIDs(orgIDs)
	if err != nil {
		return nil, err
	}
	slugs := map[string]string{}
	for _, org := range orgs {
		slugs[org.ID] = org.Slug
	}
	for _, row := range rows {
		slug, ok := slugs[row.OrgID]
		if !ok {
			return nil, sql.ErrNoRows
		}
		out = append(out, projectValue(row, slug, members[row.ID]))
	}
	return out, nil
}

func organizationProjection(q *store.Queries, row store.OrganizationRow) (model.Organization, error) {
	members, err := q.OrganizationMembers(row.ID)
	return model.Organization{ID: row.ID, Slug: row.Slug, Name: row.Data.Name, Created: storageDate(row.Data.Created), Members: members}, err
}
func userProjection(q *store.Queries, row store.UserRow) (model.User, error) {
	ids, err := q.Identities(row.ID)
	return model.User{ID: row.ID, Name: row.Data.Name, Email: row.Email, Identities: ids}, err
}
func jobProjection(q *store.Queries, row store.JobRow) (model.Job, error) {
	var cost *int64
	if row.Data.Run != nil {
		usage, err := q.Usage(row.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return model.Job{}, err
		}
		cost = usage.Data.CostCents
	}
	return jobValue(row, cost), nil
}
func jobProjections(q *store.Queries, rows []store.JobRow) ([]model.Job, error) {
	ids := []string{}
	for _, row := range rows {
		if row.Data.Run != nil {
			ids = append(ids, row.ID)
		}
	}
	costs := map[string]*int64{}
	if len(ids) > 0 {
		usage, err := q.UsageForJobs(ids)
		if err != nil {
			return nil, err
		}
		for _, u := range usage {
			costs[u.JobID] = u.Data.CostCents
		}
	}
	out := make([]model.Job, 0, len(rows))
	for _, row := range rows {
		out = append(out, jobValue(row, costs[row.ID]))
	}
	return out, nil
}
func jobValue(row store.JobRow, cost *int64) model.Job {
	j := model.Job{ID: row.ID, ProjectID: row.ProjectID, Kind: row.Kind, Status: row.Status, Created: storageDate(row.Created), UserID: row.Data.UserID, AgentID: store.ValueString(row.Data.AgentID), AgentBudgetCents: row.Data.AgentBudgetCents, Automatic: row.Data.Automatic, Error: store.ValueString(row.Data.Error), Revision: store.ValueString(row.Data.Revision), WikiStatus: store.ValueString(row.Data.WikiStatus), Unchanged: row.Data.Unchanged, Views: []model.ViewJobResult{}}
	for _, v := range row.Data.Views {
		j.Views = append(j.Views, model.ViewJobResult{ViewID: v.ViewID, Slug: v.Slug, Status: v.Status, Error: store.ValueString(v.Error)})
	}
	if row.Data.Run != nil {
		run := row.Data.Run
		j.Run = &model.AgentRun{Phase: model.ManagedPhase(store.ValueString(row.Phase)), BudgetCents: run.BudgetCents, PreparedAt: storageDate(run.PreparedAt), Deadline: storageDate(run.Deadline), CleanupComplete: row.CleanupComplete}
		j.Run.CostCents = cost
	}
	return j
}
func member(q *store.Queries, pid, uid string) (model.Project, error) {
	row, err := sqlMember(q, pid, uid)
	if err != nil {
		return model.Project{}, err
	}
	return projectProjection(q, row)
}
func owner(q *store.Queries, pid, uid string) (model.Project, error) {
	p, err := member(q, pid, uid)
	if err == nil && p.Members[uid] != "owner" {
		err = fail(403, "owner permission required")
	}
	return p, err
}
func (s *Server) check(pid, uid string) error {
	return s.Store.Read(context.Background(), func(q *store.Queries) error { _, err := sqlMember(q, pid, uid); return err })
}
func (s *Server) mutation(r *http.Request, uid string, fn func(*store.Queries) error) error {
	return s.httpMutation(r, uid, r.PathValue("project")).write(r.Context(), s.Store, fn)
}
func (s *Server) httpMutation(r *http.Request, uid, pid string) mutationHook {
	return mutationHook{
		check: func(q *store.Queries) error { return s.checkMutation(q, r, uid, pid) },
		audit: func(q *store.Queries) error {
			if pid == "" || r.Method == "DELETE" && r.Pattern == "DELETE /api/projects/{org}/{slug}" {
				return nil
			}
			return s.auditMutation(q, r, uid, pid)
		},
	}
}

func (s *Server) auditMutation(q *store.Queries, r *http.Request, uid, pid string) error {
	if _, err := q.Project(pid); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	detail := r.Pattern
	if identity, ok := r.Context().Value(authContextKey{}).(requestIdentity); ok && identity.principal.AgentID != "" {
		detail += " agent=" + identity.principal.AgentID
	}
	return recordAudit(q, uid, pid, r.Method, detail)
}

func recordAudit(q *store.Queries, uid, pid, action, detail string) error {
	if err := q.PutAudit(store.AuditRow{ID: auth.Token()[:20], ProjectID: pid, Created: time.Now().UnixMilli(), Data: store.AuditData{UserID: uid, Action: action, Detail: detail}}); err != nil {
		return err
	}
	return q.CompactHistory(pid, nil)
}

func creationDate(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return storageDate(v)
}

func wikiProjection(q *store.Queries, p store.ProjectRow, names ...string) (model.Revision, error) {
	rev := model.Revision{ID: store.ValueString(p.WikiRevision), ProjectID: p.ID, Files: map[string]blob.Key{}, FileHashes: map[string]string{}, FileBytes: map[string]int64{}}
	var rows []store.WikiFileRow
	var err error
	if len(names) > 0 && names[0] != "" {
		row, e := q.WikiFile(p.ID, names[0])
		if e == nil {
			rows = append(rows, row)
		} else if !errors.Is(e, sql.ErrNoRows) {
			return rev, e
		}
	} else {
		rows, err = q.ProjectWiki(p.ID)
	}
	if err != nil {
		return rev, err
	}
	for _, row := range rows {
		rev.Files[row.Path] = model.WikiContentKey(p.ID, model.ContentHash(row.Hash))
		rev.FileHashes[row.Path] = row.Hash
		rev.FileBytes[row.Path] = row.Data.Bytes
		rev.StoredBytes += row.Data.Bytes
	}
	return rev, nil
}

type mutationRequestKey struct{}

func (s *Server) requestHook(ctx context.Context, pid string) mutationHook {
	r, ok := ctx.Value(mutationRequestKey{}).(*http.Request)
	if !ok {
		return mutationHook{}
	}
	identity, _ := r.Context().Value(authContextKey{}).(requestIdentity)
	return s.httpMutation(r, identity.principal.UserID, pid)
}
func (s *Server) requestMutation(ctx context.Context, q *store.Queries, pid string) error {
	return s.requestHook(ctx, pid).apply(q, func(*store.Queries) error { return nil })
}
func (s *Server) write(ctx context.Context, fn func(*store.Queries) error) error {
	r, ok := ctx.Value(mutationRequestKey{}).(*http.Request)
	pid := ""
	if ok {
		pid = r.PathValue("project")
	}
	return s.requestHook(ctx, pid).write(ctx, s.Store, fn)
}
func (s *Server) checkMutation(q *store.Queries, r *http.Request, uid, pid string) error {
	identity, ok := r.Context().Value(authContextKey{}).(requestIdentity)
	if !ok {
		return nil
	}
	principal := identity.principal
	if principal.Role == "job" {
		_, err := checkSQLJobCredential(q, s.Cipher, requestBearer(r), pid, r.PathValue("job"), time.Now())
		return err
	}
	row, err := q.Session(identity.key)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(401, "session changed; sign in again")
	}
	if err != nil {
		return err
	}
	if row.UserID != uid || !row.Active(time.Now().UnixMilli()) {
		return fail(401, "session changed; sign in again")
	}
	if principal.AgentID != "" {
		data, err := store.SessionData[store.AgentSessionData](row)
		if err != nil {
			return err
		}
		logout := r.Pattern == "POST /api/logout"
		if row.Kind != "agent" || data.RevokedAt != nil || !logout && (store.ValueString(data.Role) != "write" || store.ValueString(row.ProjectID) != pid) {
			return fail(403, "agent permission denied")
		}
	}
	if _, err = q.User(uid); errors.Is(err, sql.ErrNoRows) {
		return fail(401, "session changed; sign in again")
	} else if err != nil {
		return err
	}
	if pid != "" {
		if strings.Contains(r.Pattern, "/views") {
			_, err = owner(q, pid, uid)
		} else {
			_, err = sqlMember(q, pid, uid)
		}
		return err
	}
	return nil
}
