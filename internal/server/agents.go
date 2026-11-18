package server

import (
	"net/http"
	"sort"

	"github.com/mash/kioku/internal/store"
)

const agentInputRoute = "GET /api/projects/{org}/{slug}/jobs/{job}/agent/input"
const agentOutputRoute = "PUT /api/projects/{org}/{slug}/jobs/{job}/agent/output"

func (s *Server) authorizeAgent(r *http.Request, principal authenticatedSession) error {
	jobRoute := r.Pattern == agentInputRoute || r.Pattern == agentOutputRoute
	if principal.Role == "job" {
		if !jobRoute {
			return fail(403, "job credential permits only job input and output")
		}
		// wrap validates the job credential; the I/O handler rechecks after locking.
		return nil
	}
	if jobRoute {
		return fail(403, "job credential required")
	}
	if principal.AgentID == "" {
		return nil
	}
	if pid := r.PathValue("project"); pid != "" && pid != principal.ProjectID {
		return fail(404, "project not found")
	}
	switch r.Pattern {
	case "GET /api/me", "GET /api/projects", "POST /api/logout", "GET /api/projects/{org}/{slug}/snapshot", "GET /api/projects/{org}/{slug}/views", "GET /api/projects/{org}/{slug}/views/{view}", "GET /api/projects/{org}/{slug}/jobs/{job}", "GET /api/projects/{org}/{slug}/usage":
		return nil
	case "POST /api/projects/{org}/{slug}/put", "POST /api/projects/{org}/{slug}/generate":
		if principal.Role == "write" {
			return nil
		}
	}
	return fail(403, "agent credential does not permit this operation")
}
func publicAgent(row store.SessionRow, data store.AgentSessionData) map[string]any {
	return map[string]any{"id": store.ValueString(data.AgentID), "issuer_id": row.UserID, "name": data.Name, "role": store.ValueString(data.Role), "created": creationDate(row.Created), "expires": storageDate(row.Expires)}
}
func (s *Server) agents(w http.ResponseWriter, r *http.Request, uid string) error {
	out := []map[string]any{}
	if err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		p, e := member(q, r.PathValue("project"), uid)
		if e != nil {
			return e
		}
		rows, e := q.ProjectSessions(p.ID, "agent")
		if e != nil {
			return e
		}
		for _, row := range rows {
			if row.UserID != uid && p.Members[uid] != "owner" {
				continue
			}
			data, e := store.SessionData[store.AgentSessionData](row)
			if e != nil {
				return e
			}
			out = append(out, publicAgent(row, data))
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	jsonOut(w, 200, out)
	return nil
}
func (s *Server) revokeAgent(w http.ResponseWriter, r *http.Request, uid string) error {
	if err := s.updateAccount(r, uid, func(q *store.Queries) error {
		p, e := member(q, r.PathValue("project"), uid)
		if e != nil {
			return e
		}
		rows, e := q.ProjectSessions(p.ID, "agent")
		if e != nil {
			return e
		}
		for _, row := range rows {
			data, e := store.SessionData[store.AgentSessionData](row)
			if e != nil {
				return e
			}
			if store.ValueString(data.AgentID) == r.PathValue("agent") && (row.UserID == uid || p.Members[uid] == "owner") {
				return q.DeleteSession(row.ID)
			}
		}
		return fail(404, "agent credential not found")
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"revoked": true})
	return nil
}
func revokeAgents(q *store.Queries, uid, pid string) error {
	rows, err := q.UserSessions(uid, "agent")
	if err != nil {
		return err
	}
	for _, row := range rows {
		if pid != "" && store.ValueString(row.ProjectID) != pid {
			continue
		}
		if err = q.DeleteSession(row.ID); err != nil {
			return err
		}
	}
	return nil
}
