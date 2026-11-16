package server

import (
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/mash/kioku/internal/store"
)

// Administration requires live provider proof from this exact human session.
func (s *Server) confirmAccount(r *http.Request, uid string) error {
	_, err := s.currentEmail(r, uid)
	return err
}
func (s *Server) updateAccount(r *http.Request, uid string, fn func(*store.Queries) error) error {
	key, _, err := s.session(r)
	if err != nil {
		return err
	}
	return s.mutation(r, uid, func(q *store.Queries) error {
		session, err := q.Session(key)
		if errors.Is(err, sql.ErrNoRows) {
			return fail(401, "session changed; sign in again")
		}
		if err != nil {
			return err
		}
		if session.Kind != "user" {
			return fail(401, "session changed; sign in again")
		}
		return fn(q)
	})
}
func (s *Server) account(w http.ResponseWriter, r *http.Request, uid string) error {
	current, _, err := s.session(r)
	if err != nil {
		return err
	}
	var result map[string]any
	err = s.Store.Read(r.Context(), func(q *store.Queries) error {
		u, err := q.User(uid)
		if err != nil {
			return err
		}
		ids, err := q.Identities(uid)
		if err != nil {
			return err
		}
		identities := []string{}
		for p := range ids {
			identities = append(identities, p)
		}
		sort.Strings(identities)
		sessions := []map[string]any{}
		rows, err := q.UserSessions(uid, "")
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Kind != "user" || !row.Active(time.Now().UnixMilli()) {
				continue
			}
			data, err := store.SessionData[store.IdentitySessionData](row)
			if err != nil {
				return err
			}
			sessions = append(sessions, map[string]any{"id": row.ID, "current": row.ID == current, "created": creationDate(row.Created), "expires": storageDate(row.Expires), "provider": store.ValueString(data.IdentityProvider)})
		}
		result = map[string]any{"id": u.ID, "name": u.Data.Name, "email": u.Email, "identities": identities, "sessions": sessions}
		return nil
	})
	if err != nil {
		return err
	}
	jsonOut(w, 200, result)
	return nil
}
func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request, uid string) error {
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	if err := s.updateAccount(r, uid, func(q *store.Queries) error {
		row, err := q.Session(r.PathValue("session"))
		if errors.Is(err, sql.ErrNoRows) {
			return fail(404, "session not found")
		}
		if err != nil {
			return err
		}
		if row.UserID != uid || row.Kind != "user" {
			return fail(404, "session not found")
		}
		return q.DeleteSession(row.ID)
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"revoked": true})
	return nil
}
func (s *Server) revokeAllSessions(w http.ResponseWriter, r *http.Request, uid string) error {
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	if err := s.updateAccount(r, uid, func(q *store.Queries) error { return q.RevokeUserSessions(uid) }); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"revoked": true})
	return nil
}
func (s *Server) unlinkIdentity(w http.ResponseWriter, r *http.Request, uid string) error {
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	_, principal, err := s.session(r)
	if err != nil {
		return err
	}
	provider := r.PathValue("provider")
	if err = s.updateAccount(r, uid, func(q *store.Queries) error {
		ids, err := q.Identities(uid)
		if err != nil {
			return err
		}
		if _, ok := ids[provider]; !ok {
			return fail(404, "identity not linked")
		}
		if len(ids) < 2 {
			return fail(409, "cannot unlink the last login identity")
		}
		if store.ValueString(principal.Identity.IdentityProvider) == provider {
			return fail(409, "sign in with another linked provider before unlinking this identity")
		}
		if err := q.DeleteIdentity(uid, provider); err != nil {
			return err
		}
		rows, err := q.UserSessions(uid, "")
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Kind != "user" && row.Kind != "cli_login" {
				continue
			}
			data, err := store.SessionData[store.IdentitySessionData](row)
			if err != nil {
				return err
			}
			if store.ValueString(data.IdentityProvider) == provider {
				if err := q.DeleteSession(row.ID); err != nil {
					return err
				}
			}
		}
		return revokeAgents(q, uid, "")
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"unlinked": true})
	return nil
}
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, uid string) error {
	body := readBody(w, r)
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	if err := s.updateAccount(r, uid, func(q *store.Queries) error {
		var req struct {
			Confirm string `json:"confirm"`
		}
		if err := body.decode(&req); err != nil {
			return err
		}
		if req.Confirm != "delete-account" {
			return fail(400, "explicit delete-account confirmation required")
		}
		projects, err := q.ProjectsForUser(uid)
		if err != nil {
			return err
		}
		for _, p := range projects {
			role, err := q.ProjectRole(p.ID, uid)
			if err != nil {
				return err
			}
			if role == "owner" {
				return fail(409, "transfer or delete owned projects before deleting your account")
			}
		}
		owned, err := q.OwnedOrganizations(uid)
		if err != nil {
			return err
		}
		for org := range owned {
			projects, err := q.OrganizationProjects(org)
			if err != nil {
				return err
			}
			if len(projects) > 0 {
				return fail(409, "transfer organization ownership before deleting your account")
			}
			if err := q.DeleteOrganizationRows(org); err != nil {
				return err
			}
		}
		return q.DeleteAccountRows(uid)
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"deleted": true})
	return nil
}
func decodeTransferTarget(q *store.Queries, body requestBody) (string, error) {
	var req struct {
		Target string `json:"target"`
	}
	if err := body.decode(&req); err != nil {
		return "", err
	}
	target := req.Target
	if u, err := q.User(target); err == nil {
		return u.ID, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	users, err := q.UsersByEmail(target)
	if err != nil {
		return "", err
	}
	if len(users) > 1 {
		return "", fail(409, "email is ambiguous; use the account ID")
	}
	if len(users) == 0 {
		return "", fail(404, "target account not found")
	}
	return users[0].ID, nil
}
func (s *Server) transferProject(w http.ResponseWriter, r *http.Request, uid string) error {
	body := readBody(w, r)
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	if err := s.updateAccount(r, uid, func(q *store.Queries) error {
		p, err := owner(q, r.PathValue("project"), uid)
		if err != nil {
			return err
		}
		target, err := decodeTransferTarget(q, body)
		if err != nil {
			return err
		}
		if target == uid || p.Members[target] == "" {
			return fail(409, "new owner must be another current project member")
		}
		for user, role := range p.Members {
			if role == "owner" {
				if err := q.PutProjectMember(p.ID, user, "member"); err != nil {
					return err
				}
			}
		}
		return q.PutProjectMember(p.ID, target, "owner")
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"transferred": true})
	return nil
}
func (s *Server) transferOrganization(w http.ResponseWriter, r *http.Request, uid string) error {
	body := readBody(w, r)
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	if err := s.updateAccount(r, uid, func(q *store.Queries) error {
		org, err := orgOwner(q, r.PathValue("org"), uid)
		if err != nil {
			return err
		}
		target, err := decodeTransferTarget(q, body)
		if err != nil {
			return err
		}
		eligible := org.Owners[target]
		projects, err := q.OrganizationProjects(org.ID)
		if err != nil {
			return err
		}
		for _, p := range projects {
			role, err := q.ProjectRole(p.ID, target)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if role != "" {
				eligible = true
			}
		}
		if target == uid || !eligible {
			return fail(409, "new owner must already belong to an organization project")
		}
		return q.ReplaceOrganizationOwners(org.ID, target)
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"transferred": true})
	return nil
}
