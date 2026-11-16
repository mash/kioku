package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func (s *Server) me(w http.ResponseWriter, r *http.Request, uid string) error {
	_, principal, err := s.session(r)
	if err != nil {
		return err
	}
	var out any
	if err = s.Store.Read(r.Context(), func(q *store.Queries) error {
		if principal.AgentID != "" {
			row, e := q.Project(principal.ProjectID)
			if e != nil {
				return e
			}
			p, e := projectProjection(q, row)
			if e != nil {
				return e
			}
			out = map[string]any{"agent_id": principal.AgentID, "namespace": p.Namespace, "role": principal.Role, "expires": principal.Expires}
			return nil
		}
		u, e := q.User(uid)
		if e == nil {
			out = map[string]any{"id": u.ID, "name": u.Data.Name, "email": u.Email}
		}
		return e
	}); err != nil {
		return err
	}
	jsonOut(w, 200, out)
	return nil
}
func (s *Server) projects(w http.ResponseWriter, r *http.Request, uid string) error {
	_, principal, err := s.session(r)
	if err != nil {
		return err
	}
	out := []model.ProjectEntry{}
	err = s.Store.Read(r.Context(), func(q *store.Queries) error {
		rows, e := q.ProjectsForUser(uid)
		if e != nil {
			return e
		}
		selected := rows[:0]
		ids := []string{}
		for _, row := range rows {
			if principal.AgentID != "" && row.ID != principal.ProjectID {
				continue
			}
			selected = append(selected, row)
			ids = append(ids, row.ID)
		}
		projects, e := projectProjections(q, selected)
		if e != nil {
			return e
		}
		inbox := map[string][]store.InboxRow{}
		if len(ids) > 0 {
			items, e := q.InboxForProjects(ids)
			if e != nil {
				return e
			}
			for _, item := range items {
				inbox[item.ProjectID] = append(inbox[item.ProjectID], item)
			}
		}
		for _, p := range projects {
			entry := model.ProjectEntry{Project: p, Inbox: []model.InboxEntry{}}
			for _, item := range inbox[p.ID] {
				entry.Inbox = append(entry.Inbox, model.InboxEntry{SourceURL: item.SourceURL, Remove: item.Remove})
			}
			sort.Slice(entry.Inbox, func(i, j int) bool {
				if entry.Inbox[i].SourceURL == entry.Inbox[j].SourceURL {
					return !entry.Inbox[i].Remove && entry.Inbox[j].Remove
				}
				return entry.Inbox[i].SourceURL < entry.Inbox[j].SourceURL
			})
			entry.InboxCount = len(entry.Inbox)
			out = append(out, entry)
		}
		return nil
	})
	if err != nil {
		return err
	}
	jsonOut(w, 200, out)
	return nil
}
func (s *Server) createProject(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Name         string `json:"name"`
		Organization string `json:"organization"`
		Slug         string `json:"slug"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		return fail(400, "name must contain 1–100 bytes")
	}
	if strings.Contains(req.Name, "/") {
		parts := strings.Split(req.Name, "/")
		if len(parts) != 2 || req.Organization != "" {
			return fail(400, "use organization/project")
		}
		req.Organization, req.Slug, req.Name = parts[0], parts[1], parts[1]
	}
	if req.Slug == "" {
		req.Slug = projectSlug(req.Name)
	}
	if !validSlug(req.Slug) {
		return fail(400, "invalid project slug")
	}
	row := store.ProjectRow{ID: auth.Token()[:20], Slug: req.Slug, Created: time.Now().UnixMilli(), Data: store.ProjectData{Name: req.Name}}
	// Check access before committing the organization, project and default Views.
	if _, err := s.Blobs.List(r.Context(), "projects/"+row.ID+"/"); err != nil {
		return err
	}
	var result model.Project
	if err := s.mutation(r, uid, func(q *store.Queries) error {
		var org model.Organization
		var e error
		if req.Organization == "" {
			org, e = personalOrg(q, uid)
			if e == nil && !org.Owners[uid] {
				return fail(409, "personal namespace ownership changed; select a named organization")
			}
		} else {
			org, e = orgOwner(q, req.Organization, uid)
		}
		if e != nil {
			return e
		}
		row.OrgID = org.ID
		if _, e = q.ProjectBySlug(org.ID, row.Slug); e == nil {
			return fail(409, "project name is already in use in this organization")
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e = q.PutProject(row); e != nil {
			return e
		}
		if e = q.PutProjectMember(row.ID, uid, "owner"); e != nil {
			return e
		}
		if e = seedSQLViews(q, row); e != nil {
			return e
		}
		result, e = projectProjection(q, row)
		return e
	}); err != nil {
		return err
	}
	jsonOut(w, 201, result)
	return nil
}
func seedSQLViews(q *store.Queries, p store.ProjectRow) error {
	title, prompt, status := projectViewPresets[0].Title, projectViewPresets[0].Prompt, "pending"
	return q.PutView(store.ViewRow{ProjectID: p.ID, ID: "vw_" + auth.Hash(p.ID + "/project-map")[:20], Slug: "project-map", CreatedAt: &p.Created, Data: store.ViewData{Title: &title, Prompt: &prompt, Status: &status}})
}
func (s *Server) members(w http.ResponseWriter, r *http.Request, uid string) error {
	out := []map[string]any{}
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		p, e := member(q, r.PathValue("project"), uid)
		if e != nil {
			return e
		}
		for id, role := range p.Members {
			u, e := q.User(id)
			if e != nil {
				return e
			}
			out = append(out, map[string]any{"id": id, "name": u.Data.Name, "email": u.Email, "role": role, "status": "active"})
		}
		if p.Members[uid] == "owner" {
			invites, e := q.ProjectInvites(p.ID)
			if e != nil {
				return e
			}
			for _, inv := range invites {
				if inv.UsedBy == nil && inv.Expires > time.Now().UnixMilli() {
					out = append(out, map[string]any{"id": inv.ID, "email": store.ValueString(inv.Data.Email), "role": "member", "status": "invited", "expires": storageDate(inv.Expires)})
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]["id"]) < fmt.Sprint(out[j]["id"]) })
	jsonOut(w, 200, out)
	return nil
}
func (s *Server) invite(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Email string `json:"email"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Email != "" && (!strings.Contains(req.Email, "@") || len(req.Email) > 254) {
		return fail(400, "invalid email")
	}
	token := auth.Token()
	inv := store.InviteRow{ID: auth.Token()[:20], ProjectID: r.PathValue("project"), CreatedBy: uid, Expires: time.Now().Add(7 * 24 * time.Hour).UnixMilli(), TokenHash: auth.Hash(token), Data: store.InviteData{Email: store.NullableString(req.Email)}}
	if err := s.mutation(r, uid, func(q *store.Queries) error {
		if _, e := owner(q, inv.ProjectID, uid); e != nil {
			return e
		}
		return q.PutInvite(inv)
	}); err != nil {
		return err
	}
	jsonOut(w, 201, map[string]any{"url": s.Config.BaseURL + "/invite/" + token, "expires": storageDate(inv.Expires), "email": req.Email})
	return nil
}
func activeInvite(q *store.Queries, hash string) (store.InviteRow, error) {
	inv, e := q.InviteByToken(hash)
	if errors.Is(e, sql.ErrNoRows) {
		return inv, fail(404, "invite not found")
	}
	if e != nil {
		return inv, e
	}
	if inv.UsedBy != nil || inv.Expires <= time.Now().UnixMilli() {
		return inv, fail(410, "invite expired or already used")
	}
	return inv, nil
}
func (s *Server) join(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if u, e := url.Parse(req.Token); e == nil && u.Host != "" {
		req.Token = strings.TrimPrefix(u.Path, "/invite/")
	}
	hash := auth.Hash(req.Token)
	var boundEmail string
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		inv, e := activeInvite(q, hash)
		boundEmail = store.ValueString(inv.Data.Email)
		return e
	}); err != nil {
		return err
	}
	verified := ""
	if boundEmail != "" {
		var err error
		verified, err = s.currentEmail(r, uid)
		if err != nil {
			return err
		}
	}
	var result model.Project
	if err := s.mutation(r, uid, func(q *store.Queries) error {
		inv, e := activeInvite(q, hash)
		if e != nil {
			return e
		}
		if inv.Data.Email != nil && !strings.EqualFold(*inv.Data.Email, verified) {
			return fail(403, "invite is restricted to a different verified email")
		}
		p, e := q.Project(inv.ProjectID)
		if errors.Is(e, sql.ErrNoRows) {
			return fail(404, "project not found")
		}
		if e != nil {
			return e
		}
		if p.Deleting {
			return fail(409, "project deletion is in progress")
		}
		if _, e = q.ProjectRole(p.ID, uid); errors.Is(e, sql.ErrNoRows) {
			if e = q.PutProjectMember(p.ID, uid, "member"); e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		inv.UsedBy = &uid
		if e = q.PutInvite(inv); e != nil {
			return e
		}
		result, e = projectProjection(q, p)
		return e
	}); err != nil {
		return err
	}
	jsonOut(w, 200, result)
	return nil
}
func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, uid string) error {
	err := s.mutation(r, uid, func(q *store.Queries) error {
		p, e := owner(q, r.PathValue("project"), uid)
		if e != nil {
			return e
		}
		target := r.PathValue("member")
		matches, e := q.UsersByEmail(target)
		if e != nil {
			return e
		}
		for _, user := range matches {
			if p.Members[user.ID] != "" {
				target = user.ID
				break
			}
		}
		if p.Members[target] == "owner" {
			return fail(400, "cannot remove the project owner")
		}
		if p.Members[target] == "" {
			return fail(404, "member not found")
		}
		if e = q.DeleteProjectMember(p.ID, target); e != nil {
			return e
		}
		return revokeAgents(q, target, p.ID)
	})
	if err == nil {
		jsonOut(w, 200, map[string]bool{"removed": true})
	}
	return err
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request, uid string) error {
	key, principal, err := s.session(r)
	if err != nil {
		return err
	}
	if err = s.Store.Write(r.Context(), func(q *store.Queries) error {
		return q.DeleteSession(key)
	}); err != nil {
		return err
	}
	if principal.AgentID == "" {
		http.SetCookie(w, &http.Cookie{Name: "kioku_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(s.Config.BaseURL, "https://"), SameSite: http.SameSiteLaxMode})
	}
	jsonOut(w, 200, map[string]bool{"logged_out": true})
	return nil
}
