package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

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
		return s.Store.Read(r.Context(), func(q *store.Queries) error {
			_, e := checkSQLJobCredential(q, s.Cipher, requestBearer(r), r.PathValue("project"), r.PathValue("job"), time.Now())
			return e
		})
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
	case "GET /api/me", "GET /api/projects", "POST /api/logout", "GET /api/projects/{org}/{slug}/snapshot", "GET /api/projects/{org}/{slug}/views/{view}", "GET /api/projects/{org}/{slug}/jobs/{job}", "GET /api/projects/{org}/{slug}/usage":
		return nil
	case "POST /api/projects/{org}/{slug}/put", "POST /api/projects/{org}/{slug}/generate":
		if principal.Role == "write" {
			return nil
		}
	}
	return fail(403, "agent credential does not permit this operation")
}
func publicAgent(row store.SessionRow, data store.AgentSessionData) map[string]any {
	return map[string]any{"id": valueString(data.AgentID), "issuer_id": row.UserID, "name": data.Name, "role": valueString(data.Role), "created": creationDate(row.Created), "expires": storageDate(row.Expires), "revoked_at": optionalDate(data.RevokedAt)}
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
			var data store.AgentSessionData
			if e = json.Unmarshal(row.Data, &data); e != nil {
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
func revokeAgentSession(q *store.Queries, key string, now int64) error {
	row, err := q.Session(key)
	if err != nil {
		return err
	}
	if row.Kind != "agent" {
		return fail(404, "agent credential not found")
	}
	var data store.AgentSessionData
	if err = json.Unmarshal(row.Data, &data); err != nil {
		return err
	}
	if data.RevokedAt != nil {
		return nil
	}
	data.RevokedAt = &now
	row.Data, err = json.Marshal(data)
	if err != nil {
		return err
	}
	return q.PutSession(row)
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
			var data store.AgentSessionData
			if e = json.Unmarshal(row.Data, &data); e != nil {
				return e
			}
			if valueString(data.AgentID) == r.PathValue("agent") && (row.UserID == uid || p.Members[uid] == "owner") {
				return revokeAgentSession(q, row.ID, time.Now().UnixMilli())
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
		if pid != "" && valueString(row.ProjectID) != pid {
			continue
		}
		if err = revokeAgentSession(q, row.ID, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}
