package server

import (
	"crypto/subtle"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func activeAgent(st model.State, id string, now time.Time) (model.AgentToken, bool) {
	token, ok := st.AgentTokens[id]
	if !ok || !token.RevokedAt.IsZero() || !now.Before(token.Expires) {
		return token, false
	}
	if _, ok = st.Users[token.UserID]; !ok {
		return token, false
	}
	p, ok := st.Projects[token.ProjectID]
	return token, ok && !p.Deleting && p.Members[token.UserID] != ""
}
func agentFromBearer(st model.State, raw string) (model.Session, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] != "kioku_agent" {
		return model.Session{}, false
	}
	token, ok := activeAgent(st, parts[1], time.Now())
	if !ok || subtle.ConstantTimeCompare([]byte(auth.Hash(raw)), []byte(token.TokenHash)) != 1 {
		return model.Session{}, false
	}
	return model.Session{JobID: token.JobID, AgentID: token.ID, ProjectID: token.ProjectID, Role: token.Role, UserID: token.UserID, Created: token.Created, Expires: token.Expires}, true
}
func (s *Server) authorizeAgent(r *http.Request, principal model.Session) error {
	jobRoute := r.Pattern == agentInputRoute || r.Pattern == agentOutputRoute
	if principal.Role == "job" {
		if !jobRoute {
			return fail(403, "job credential permits only job input and output")
		}
		return s.DB.View(func(st model.State) error {
			return checkJobToken(st, principal, r.PathValue("project"), r.PathValue("job"))
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
	case "GET /api/me", "GET /api/projects", "POST /api/logout",
		"GET /api/projects/{project}/snapshot", "GET /api/projects/{project}/inbox", "GET /api/projects/{project}/sources",
		"GET /api/projects/{project}/sources/{source}/{format}", "GET /api/projects/{project}/jobs/{job}", "GET /api/projects/{project}/usage":
		return nil
	case "POST /api/projects/{project}/put", "POST /api/projects/{project}/generate", "POST /api/projects/{project}/jobs/{job}/retry":
		if principal.Role == "write" {
			return nil
		}
	}
	return fail(403, "agent credential does not permit this operation")
}
func publicAgent(t model.AgentToken) map[string]any {
	return map[string]any{"id": t.ID, "project_id": t.ProjectID, "issuer_id": t.UserID, "name": t.Name, "role": t.Role, "created": t.Created, "expires": t.Expires, "revoked_at": t.RevokedAt}
}
func mintAgent(st *model.State, uid, pid, name, role string, lifetime time.Duration) (string, model.AgentToken, error) {
	if _, err := member(*st, pid, uid); err != nil {
		return "", model.AgentToken{}, err
	}
	now := time.Now().UTC()
	count := 0
	for _, token := range st.AgentTokens {
		if token.ProjectID == pid && token.RevokedAt.IsZero() && token.Expires.After(now) {
			count++
		}
	}
	if count >= 100 {
		return "", model.AgentToken{}, fail(429, "project has too many active agent credentials")
	}
	id := auth.Token()[:20]
	raw := "kioku_agent." + id + "." + auth.Token()
	token := model.AgentToken{ID: id, UserID: uid, ProjectID: pid, Name: name, Role: role, Created: now, Expires: now.Add(lifetime), TokenHash: auth.Hash(raw)}
	if st.AgentTokens == nil {
		st.AgentTokens = map[string]model.AgentToken{}
	}
	st.AgentTokens[id] = token
	return raw, token, nil
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request, uid string) error {
	out := []map[string]any{}
	err := s.DB.View(func(st model.State) error {
		p, err := member(st, r.PathValue("project"), uid)
		if err != nil {
			return err
		}
		for _, t := range st.AgentTokens {
			if t.ProjectID == p.ID && (t.UserID == uid || p.Members[uid] == "owner") {
				out = append(out, publicAgent(t))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	jsonOut(w, 200, out)
	return nil
}
func (s *Server) revokeAgent(w http.ResponseWriter, r *http.Request, uid string) error {
	if err := s.updateAccount(r, uid, func(st *model.State) error {
		p, err := member(*st, r.PathValue("project"), uid)
		if err != nil {
			return err
		}
		token, ok := st.AgentTokens[r.PathValue("agent")]
		if !ok || token.ProjectID != p.ID || (token.UserID != uid && p.Members[uid] != "owner") {
			return fail(404, "agent credential not found")
		}
		if token.RevokedAt.IsZero() {
			token.RevokedAt = time.Now().UTC()
			st.AgentTokens[token.ID] = token
		}
		return nil
	}); err != nil {
		return err
	}
	jsonOut(w, 200, map[string]bool{"revoked": true})
	return nil
}
func revokeAgents(st *model.State, uid, pid string) {
	for id, t := range st.AgentTokens {
		if t.UserID == uid && (pid == "" || t.ProjectID == pid) && t.RevokedAt.IsZero() {
			t.RevokedAt = time.Now().UTC()
			st.AgentTokens[id] = t
		}
	}
}
func checkJobAgent(st model.State, job model.Job) error {
	if job.AgentID == "" {
		return nil
	}
	token, ok := activeAgent(st, job.AgentID, time.Now())
	if !ok || token.ProjectID != job.ProjectID || token.UserID != job.UserID || token.Role != "write" {
		return fail(403, "job agent credential expired or was revoked")
	}
	return nil
}

const agentInputRoute = "GET /api/projects/{project}/jobs/{job}/agent/input"
const agentOutputRoute = "PUT /api/projects/{project}/jobs/{job}/agent/output"

func issueJobToken(st *model.State, job model.Job, expires time.Time) (string, model.AgentToken) {
	id := auth.Token()[:20]
	raw := "kioku_agent." + id + "." + auth.Token()
	token := model.AgentToken{ID: id, JobID: job.ID, ProjectID: job.ProjectID, UserID: job.UserID, Name: "job:" + job.ID, Role: "job", Created: time.Now().UTC(), Expires: expires, TokenHash: auth.Hash(raw)}
	if st.AgentTokens == nil {
		st.AgentTokens = map[string]model.AgentToken{}
	}
	st.AgentTokens[id] = token
	return raw, token
}
func revokeJobToken(st *model.State, jobID string) {
	for id, t := range st.AgentTokens {
		if t.Role == "job" && t.JobID == jobID && t.RevokedAt.IsZero() {
			t.RevokedAt = time.Now().UTC()
			st.AgentTokens[id] = t
		}
	}
}
func checkJobToken(st model.State, p model.Session, pid, jobID string) error {
	t, ok := activeAgent(st, p.AgentID, time.Now())
	if !ok || t.Role != "job" || t.ProjectID != pid || t.JobID != jobID || p.ProjectID != pid || p.JobID != jobID {
		return fail(403, "invalid job credential")
	}
	return nil
}
