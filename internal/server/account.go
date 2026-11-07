package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/model"
)

// Administration requires live provider proof from this exact human session.
func (s *Server) confirmAccount(r *http.Request, uid string) error {
	_, err := s.currentEmail(r, uid)
	return err
}
func (s *Server) updateAccount(r *http.Request, uid string, fn func(*model.State) error) error {
	key, principal := s.session(r)
	return s.DB.Update(func(st *model.State) error {
		session, ok := st.Sessions[key]
		if _, exists := st.Users[uid]; !exists || !ok || session.UserID != uid || !time.Now().Before(session.Expires) || principal.AgentID != "" {
			return fail(401, "session changed; sign in again")
		}
		return fn(st)
	})
}

func (s *Server) account(w http.ResponseWriter, r *http.Request, uid string) error {
	current, _ := s.session(r)
	err := s.DB.View(func(st model.State) error {
		u := st.Users[uid]
		identities := []string{}
		connections := []string{}
		sessions := []map[string]any{}
		for provider := range u.Identities {
			identities = append(identities, provider)
		}
		for provider := range u.Connections {
			connections = append(connections, provider)
		}
		for id, session := range st.Sessions {
			if session.UserID == uid && session.Expires.After(time.Now()) {
				sessions = append(sessions, map[string]any{"id": id, "current": id == current, "created": session.Created, "expires": session.Expires, "provider": session.IdentityProvider})
			}
		}
		sort.Strings(identities)
		sort.Strings(connections)
		sort.Slice(sessions, func(i, j int) bool { return sessions[i]["id"].(string) < sessions[j]["id"].(string) })
		jsonOut(w, 200, map[string]any{"id": u.ID, "name": u.Name, "email": u.Email, "identities": identities, "connections": connections, "sessions": sessions})
		return nil
	})
	return err
}
func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request, uid string) error {
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	if err := s.updateAccount(r, uid, func(st *model.State) error {
		key := r.PathValue("session")
		session, ok := st.Sessions[key]
		if !ok || session.UserID != uid {
			return fail(404, "session not found")
		}
		delete(st.Sessions, key)
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"revoked": true})
	return nil
}
func revokeAccountAccess(st *model.State, uid string) {
	for id, session := range st.Sessions {
		if session.UserID == uid {
			delete(st.Sessions, id)
		}
	}
	for id, device := range st.Devices {
		if device.UserID == uid {
			delete(st.Devices, id)
		}
	}
	for id, pending := range st.OAuth {
		if pending.UserID == uid {
			delete(st.OAuth, id)
		}
	}
	for key, grant := range st.OAuthTokens {
		if grant.UserID == uid {
			delete(st.OAuthTokens, key)
		}
	}
	for key, code := range st.OAuthCodes {
		if code.UserID == uid {
			delete(st.OAuthCodes, key)
		}
	}
	for key, code := range st.LoginCodes {
		if code.UserID == uid {
			delete(st.LoginCodes, key)
		}
	}
	revokeAgents(st, uid, "")
}
func (s *Server) revokeAllSessions(w http.ResponseWriter, r *http.Request, uid string) error {
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	if err := s.updateAccount(r, uid, func(st *model.State) error { revokeAccountAccess(st, uid); return nil }); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"revoked": true})
	return nil
}
func (s *Server) disconnectProvider(w http.ResponseWriter, r *http.Request, uid string) error {
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	provider := r.PathValue("provider")
	if provider != "google" && provider != "github" {
		return fail(400, "unknown provider")
	}
	if err := s.updateAccount(r, uid, func(st *model.State) error {
		user := st.Users[uid]
		delete(user.Connections, provider)
		st.Users[uid] = user
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"disconnected": true})
	return nil
}
func (s *Server) unlinkIdentity(w http.ResponseWriter, r *http.Request, uid string) error {
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	_, principal := s.session(r)
	provider := r.PathValue("provider")
	if err := s.updateAccount(r, uid, func(st *model.State) error {
		user := st.Users[uid]
		if _, exists := user.Identities[provider]; !exists {
			return fail(404, "identity not linked")
		}
		if len(user.Identities) < 2 {
			return fail(409, "cannot unlink the last login identity")
		}
		if principal.IdentityProvider == provider {
			return fail(409, "sign in with another linked provider before unlinking this identity")
		}
		delete(user.Identities, provider)
		delete(user.Connections, provider)
		st.Users[uid] = user
		for id, session := range st.Sessions {
			if session.UserID == uid && session.IdentityProvider == provider {
				delete(st.Sessions, id)
			}
		}
		for id, device := range st.Devices {
			if device.UserID == uid && device.IdentityProvider == provider {
				delete(st.Devices, id)
			}
		}
		for id, pending := range st.OAuth {
			if pending.UserID == uid && pending.Provider == provider {
				delete(st.OAuth, id)
			}
		}
		revokeAgents(st, uid, "")
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"unlinked": true})
	return nil
}
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if req.Confirm != "delete-account" {
		return fail(400, "explicit delete-account confirmation required")
	}
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	if err := s.updateAccount(r, uid, func(st *model.State) error {
		for _, p := range st.Projects {
			if p.Members[uid] == "owner" {
				return fail(409, "transfer or delete owned projects before deleting your account")
			}
		}
		for id, org := range st.Organizations {
			if org.Members[uid] != "owner" {
				continue
			}
			for _, p := range st.Projects {
				if p.OrgID == id {
					return fail(409, "transfer organization ownership before deleting your account")
				}
			}
			delete(st.Organizations, id) // Empty organizations need no handoff.
		}
		for id, p := range st.Projects {
			if p.Members[uid] != "" {
				delete(p.Members, uid)
				st.Projects[id] = p
			}
		}
		for id, org := range st.Organizations {
			if org.Members[uid] != "" {
				delete(org.Members, uid)
				st.Organizations[id] = org
			}
		}
		for id, invite := range st.Invites {
			if invite.CreatedBy == uid && invite.UsedBy == "" {
				delete(st.Invites, id)
			}
		}
		revokeAccountAccess(st, uid)
		delete(st.Users, uid)
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"deleted": true})
	return nil
}
func transferTarget(st model.State, target string) (string, error) {
	if _, exists := st.Users[target]; exists {
		return target, nil
	}
	id := ""
	for uid, u := range st.Users {
		if strings.EqualFold(u.Email, target) {
			if id != "" {
				return "", fail(409, "email is ambiguous; use the account ID")
			}
			id = uid
		}
	}
	if id == "" {
		return "", fail(404, "target account not found")
	}
	return id, nil
}
func (s *Server) transferProject(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Target string `json:"target"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	if err := s.updateAccount(r, uid, func(st *model.State) error {
		p, err := owner(*st, r.PathValue("project"), uid)
		if err != nil {
			return err
		}
		target, err := transferTarget(*st, req.Target)
		if err != nil {
			return err
		}
		if target == uid || p.Members[target] == "" {
			return fail(409, "new owner must be another current project member")
		}
		for member, role := range p.Members {
			if role == "owner" {
				p.Members[member] = "member"
			}
		}
		p.Members[target] = "owner"
		st.Projects[p.ID] = p
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"transferred": true})
	return nil
}

func (s *Server) transferOrganization(w http.ResponseWriter, r *http.Request, uid string) error {
	var req struct {
		Target string `json:"target"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := s.confirmAccount(r, uid); err != nil {
		return err
	}
	if err := s.updateAccount(r, uid, func(st *model.State) error {
		org, err := orgOwner(*st, r.PathValue("org"), uid)
		if err != nil {
			return err
		}
		target, err := transferTarget(*st, req.Target)
		if err != nil {
			return err
		}
		eligible := org.Members[target] != ""
		for _, p := range st.Projects {
			if p.OrgID == org.ID && p.Members[target] != "" {
				eligible = true
			}
		}
		if target == uid || !eligible {
			return fail(409, "new owner must already belong to an organization project")
		}
		for member, role := range org.Members {
			if role == "owner" {
				org.Members[member] = "member"
			}
		}
		org.Members[target] = "owner"
		st.Organizations[org.ID] = org
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"transferred": true})
	return nil
}
