package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func validSlug(s string) bool { return model.ValidSlug(s) }
func reservedNamespace(s string) bool {
	if strings.HasPrefix(s, "u-") {
		return true
	}
	switch s {
	case "api", "auth", "invite", "projects", "healthz", "metrics", "webhooks", "settings", "billing":
		return true
	}
	return false
}

func projectSlug(name string) string {
	s := strings.ToLower(name)
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	if !validSlug(out) {
		out = "project-" + auth.Hash(name)[:12]
	}
	return out
}
func orgOwner(q *store.Queries, slug, uid string) (model.Organization, error) {
	row, err := q.OrganizationBySlug(slug)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Organization{}, fail(404, "organization not found or owner permission required")
	}
	if err != nil {
		return model.Organization{}, err
	}
	role, err := q.OrganizationRole(row.ID, uid)
	if errors.Is(err, sql.ErrNoRows) || err == nil && role != "owner" {
		return model.Organization{}, fail(404, "organization not found or owner permission required")
	}
	if err != nil {
		return model.Organization{}, err
	}
	return organizationProjection(q, row)
}
func personalOrg(q *store.Queries, uid string) (model.Organization, error) {
	id := "personal-" + uid
	row, err := q.Organization(id)
	if err == nil {
		return organizationProjection(q, row)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Organization{}, err
	}
	row = store.OrganizationRow{ID: id, Slug: "u-" + auth.Hash(uid)[:12], Data: store.OrganizationData{Name: "Personal projects", Created: time.Now().UnixMilli()}}
	if err = q.PutOrganization(row); err != nil {
		return model.Organization{}, err
	}
	if err = q.PutOrganizationMember(id, uid, "owner"); err != nil {
		return model.Organization{}, err
	}
	return organizationProjection(q, row)
}
func (s *Server) createOrganization(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if !validSlug(req.Slug) || reservedNamespace(req.Slug) {
		return fail(400, "organization slug must be lowercase letters/numbers with single hyphens; reserved route names are unavailable")
	}
	if req.Name == "" {
		req.Name = req.Slug
	}
	if len(req.Name) > 100 {
		return fail(400, "organization name exceeds 100 bytes")
	}
	org := store.OrganizationRow{ID: auth.Token()[:20], Slug: req.Slug, Data: store.OrganizationData{Name: req.Name, Created: time.Now().UnixMilli()}}
	if err := s.mutation(r, uid, func(q *store.Queries) error {
		orgs, e := q.OrganizationsForUser(uid)
		if e != nil {
			return e
		}
		owned := 0
		for _, o := range orgs {
			role, e := q.OrganizationRole(o.ID, uid)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if role == "owner" {
				owned++
			}
		}
		if owned >= 20 {
			return fail(429, "organization ownership limit reached")
		}
		if _, e = q.OrganizationBySlug(req.Slug); e == nil {
			return fail(409, "organization slug is already in use")
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e = q.PutOrganization(org); e != nil {
			return e
		}
		return q.PutOrganizationMember(org.ID, uid, "owner")
	}); err != nil {
		return err
	}
	jsonOut(w, 201, map[string]any{"id": org.ID, "slug": org.Slug, "name": org.Data.Name})
	return nil
}
func (s *Server) organizations(w http.ResponseWriter, r *http.Request, uid string) error {
	out := []map[string]string{}
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		orgs, e := q.OrganizationsForUser(uid)
		if e != nil {
			return e
		}
		for _, org := range orgs {
			role, e := q.OrganizationRole(org.ID, uid)
			if errors.Is(e, sql.ErrNoRows) {
				role = "member"
			} else if e != nil {
				return e
			}
			out = append(out, map[string]string{"id": org.ID, "slug": org.Slug, "name": org.Data.Name, "role": role})
		}
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 200, out)
	return nil
}
func namespaceProject(q *store.Queries, orgSlug, slug, uid string, allowDeleting ...bool) (model.Project, error) {
	org, err := q.OrganizationBySlug(orgSlug)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Project{}, fail(404, "project not found")
	}
	if err != nil {
		return model.Project{}, err
	}
	p, err := q.ProjectBySlug(org.ID, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Project{}, fail(404, "project not found")
	}
	if err != nil {
		return model.Project{}, err
	}
	if len(allowDeleting) > 0 && allowDeleting[0] {
		if err := deleteOwner(q, p.ID, uid); err != nil {
			return model.Project{}, err
		}
		if _, e := q.User(uid); e != nil {
			return model.Project{}, e
		}
		return projectProjection(q, p)
	}
	projected, e := projectProjection(q, p)
	if e != nil {
		return projected, e
	}
	_, e = sqlMember(q, p.ID, uid)
	return projected, e
}
func (s *Server) namespacePage(w http.ResponseWriter, r *http.Request, uid string) error {
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		p, e := namespaceProject(q, r.PathValue("org"), r.PathValue("slug"), uid)
		if e == nil {
			r.SetPathValue("project", p.ID)
		}
		return e
	}); err != nil {
		return err
	}
	return s.projectPage(w, r, uid)
}
func (s *Server) Prepare() error {
	s.prepareOnce.Do(func() {
		if s.Config.Prompts == nil {
			s.prepareErr = errors.New("prompts are not configured")
			return
		}
		if s.runners == nil {
			s.prepareErr = errors.New("runners are not configured")
		}
	})
	if s.prepareErr != nil {
		return s.prepareErr
	}
	return s.Store.Read(context.Background(), func(q *store.Queries) error { return q.Healthy() })
}
