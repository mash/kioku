package server

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
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
func orgBySlug(st model.State, slug string) (model.Organization, bool) {
	for _, org := range st.Organizations {
		if org.Slug == slug {
			return org, true
		}
	}
	return model.Organization{}, false
}
func orgOwner(st model.State, slug, uid string) (model.Organization, error) {
	org, ok := orgBySlug(st, slug)
	if !ok || org.Members[uid] != "owner" {
		return org, fail(404, "organization not found or owner permission required")
	}
	return org, nil
}
func initializeOrganizations(st *model.State) {
	if st.Organizations == nil {
		st.Organizations = map[string]model.Organization{}
	}
}
func personalOrg(st *model.State, uid string) model.Organization {
	initializeOrganizations(st)
	id := "personal-" + uid
	if org, ok := st.Organizations[id]; ok {
		return org
	}
	org := model.Organization{ID: id, Slug: "u-" + auth.Hash(uid)[:12], Name: "Personal projects", Members: map[string]string{uid: "owner"}, Created: time.Now().UTC()}
	st.Organizations[id] = org
	return org
}
func (s *Server) migrateOrganizations() error {
	return s.DB.Update(func(st *model.State) error {
		initializeOrganizations(st)
		for id, p := range st.Projects {
			if p.OrgID != "" {
				continue
			}
			ownerID := ""
			owners := []string{}
			for uid, role := range p.Members {
				if role == "owner" {
					owners = append(owners, uid)
				}
			}
			sort.Strings(owners)
			if len(owners) == 0 {
				return fmt.Errorf("project %s has no owner", id)
			}
			ownerID = owners[0]
			org := personalOrg(st, ownerID)
			slug := projectSlug(p.Name)
			for _, other := range st.Projects {
				if other.OrgID == org.ID && other.Slug == slug {
					if len(slug) > 54 {
						slug = strings.TrimRight(slug[:54], "-")
					}
					slug = slug + "-" + auth.Hash(id)[:8]
					break
				}
			}
			p.OrgID = org.ID
			p.Slug = slug
			p.Namespace = org.Slug + "/" + slug
			st.Projects[id] = p
		}
		return nil
	})
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
	org := model.Organization{ID: auth.Token()[:20], Slug: req.Slug, Name: req.Name, Members: map[string]string{uid: "owner"}, Created: time.Now().UTC()}
	if err := s.DB.Update(func(st *model.State) error {
		initializeOrganizations(st)
		owned := 0
		for _, v := range st.Organizations {
			if v.Members[uid] == "owner" {
				owned++
			}
		}
		if owned >= 20 {
			return fail(429, "organization ownership limit reached")
		}
		if _, ok := orgBySlug(*st, org.Slug); ok {
			return fail(409, "organization slug is already in use")
		}
		st.Organizations[org.ID] = org
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 201, map[string]any{"id": org.ID, "slug": org.Slug, "name": org.Name})
	return nil
}
func (s *Server) organizations(w http.ResponseWriter, r *http.Request, uid string) error {
	out := []map[string]string{}
	if err := s.DB.View(func(st model.State) error {
		for _, org := range st.Organizations {
			role := org.Members[uid]
			if role == "" {
				for _, p := range st.Projects {
					if p.OrgID == org.ID && p.Members[uid] != "" {
						role = "member"
						break
					}
				}
			}
			if role != "" {
				out = append(out, map[string]string{"id": org.ID, "slug": org.Slug, "name": org.Name, "role": role})
			}
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["slug"] < out[j]["slug"] })
	jsonOut(w, 200, out)
	return nil
}
func namespaceProject(st model.State, orgSlug, projectSlug, uid string) (model.Project, error) {
	org, ok := orgBySlug(st, orgSlug)
	if ok {
		for id, p := range st.Projects {
			if p.OrgID == org.ID && p.Slug == projectSlug {
				return member(st, id, uid)
			}
		}
	}
	return model.Project{}, fail(404, "project not found")
}

func (s *Server) namespacePage(w http.ResponseWriter, r *http.Request, uid string) error {
	pid := ""
	if err := s.DB.View(func(st model.State) error {
		p, err := namespaceProject(st, r.PathValue("org"), r.PathValue("slug"), uid)
		pid = p.ID
		return err
	}); err != nil {
		return err
	}
	r.SetPathValue("project", pid)
	return s.projectPage(w, r, uid)
}

func (s *Server) Prepare() error {
	s.prepareOnce.Do(func() {
		if s.prepareErr = s.GitHubApp.Recover(); s.prepareErr != nil {
			return
		}
		if s.runners == nil {
			s.prepareErr = errors.New("runners are not configured")
			return
		}
		if s.prepareErr = s.migrateOrganizations(); s.prepareErr != nil {
			return
		}
		s.prepareErr = s.DB.Update(func(st *model.State) error {
			for id, job := range st.Jobs {
				if job.Status == "running" && job.Run == nil {
					job.Status = "queued"
					st.Jobs[id] = job
				}
			}
			return nil
		})
	})
	return s.prepareErr
}
