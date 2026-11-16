package server

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

// exchangeCLILogin consumes even an incorrect code/PKCE pair, but SQL failure
// rolls back consumption and issuance together (A20).
func exchangeCLILogin(q *store.Queries, device, code, verifier, token string, now time.Time) (store.SessionRow, error) {
	id := auth.Hash(device)
	bad := fail(410, "login code expired or already used")
	ok, err := q.ConsumeSession(id, "cli_login", now.UnixMilli())
	if err != nil {
		return store.SessionRow{}, err
	}
	if !ok {
		v, err := q.Session(id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return store.SessionRow{}, err
		}
		if err == nil && v.Kind == "cli_login" && v.Expires <= now.UnixMilli() {
			if err := q.DeleteSession(id); err != nil {
				return store.SessionRow{}, err
			}
		}
		return store.SessionRow{}, store.RejectAfterCommit(bad)
	}
	v, err := q.Session(id)
	if err != nil {
		return store.SessionRow{}, err
	}
	data, err := store.SessionData[store.CLILoginData](v)
	if err != nil {
		return store.SessionRow{}, err
	}
	if v.UserID == "" || data.LoopbackCodeHash == nil || *data.LoopbackCodeHash != auth.Hash(code) || data.LoopbackChallenge == nil || *data.LoopbackChallenge != auth.Challenge(verifier) {
		return store.SessionRow{}, store.RejectAfterCommit(bad)
	}
	if _, err = q.User(v.UserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.SessionRow{}, store.RejectAfterCommit(bad)
		}
		return store.SessionRow{}, err
	}
	raw, err := store.EncodeSessionData(data.IdentitySessionData)
	if err != nil {
		return store.SessionRow{}, err
	}
	issued := store.SessionRow{ID: auth.Hash(token), Kind: "cli", Status: "active", UserID: v.UserID, Expires: now.Add(30 * 24 * time.Hour).UnixMilli(), Created: now.UnixMilli(), Data: raw}
	if err = q.InsertSession(issued, now.UnixMilli()); err != nil {
		return store.SessionRow{}, err
	}
	return issued, nil
}

func exchangeAgentLogin(q *store.Queries, code, name, namespace, agentID, secret string, now time.Time) (store.SessionRow, string, error) {
	v, err := q.TakeLoginCode(auth.Hash(code), now.UnixMilli())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.SessionRow{}, "", fail(401, "login code expired or already used")
		}
		return store.SessionRow{}, "", err
	}
	reject := func(e error) (store.SessionRow, string, error) {
		return store.SessionRow{}, "", store.RejectAfterCommit(e)
	}
	name = strings.TrimSpace(name)
	if len(name) < 1 || len(name) > 100 {
		return reject(fail(400, "name must contain 1–100 bytes"))
	}
	data, err := store.SessionData[store.LoginCodeData](v)
	if err != nil {
		return store.SessionRow{}, "", err
	}
	if v.ProjectID == nil {
		return reject(fail(404, "project not found"))
	}
	grant, err := q.Session(data.GrantKey)
	if errors.Is(err, sql.ErrNoRows) {
		return reject(fail(404, "project not found"))
	}
	if err != nil {
		return store.SessionRow{}, "", err
	}
	access, err := store.SessionData[store.MCPData](grant)
	if err != nil {
		return store.SessionRow{}, "", err
	}
	if grant.Kind != "mcp" || grant.UserID != v.UserID || !access.Active || access.TokenKind != "access" || !slices.Contains(access.ProjectIDs, *v.ProjectID) {
		return reject(fail(404, "project not found"))
	}
	p, err := sqlMember(q, *v.ProjectID, v.UserID)
	if err != nil {
		var api apiError
		if errors.As(err, &api) {
			return reject(err)
		}
		return store.SessionRow{}, "", err
	}
	org, err := q.Organization(p.OrgID)
	if err != nil {
		return store.SessionRow{}, "", err
	}
	resolved := org.Slug + "/" + p.Slug
	if namespace != "" && namespace != resolved {
		return reject(fail(409, "project does not match working directory"))
	}
	name = "connector:" + name
	agents, err := q.ProjectSessions(p.ID, "agent")
	if err != nil {
		return store.SessionRow{}, "", err
	}
	active := 0
	var replaced []string
	for _, old := range agents {
		if !old.Active(now.UnixMilli()) {
			continue
		}
		a, err := store.SessionData[store.AgentSessionData](old)
		if err != nil {
			return store.SessionRow{}, "", err
		}
		if a.RevokedAt != nil {
			continue
		}
		if old.UserID == v.UserID && a.Name == name {
			replaced = append(replaced, old.ID)
		} else {
			active++
		}
	}
	if active >= 100 {
		return reject(fail(429, "too many active project agents"))
	}
	raw := "kioku_agent." + agentID + "." + secret
	payload, err := store.EncodeSessionData(store.AgentSessionData{SessionRoleData: store.SessionRoleData{AgentID: &agentID, Role: data.Role}, Name: name})
	if err != nil {
		return store.SessionRow{}, "", err
	}
	issued := store.SessionRow{ID: auth.Hash(raw), Kind: "agent", Status: "active", UserID: v.UserID, ProjectID: &p.ID, Expires: now.Add(24 * time.Hour).UnixMilli(), Created: now.UnixMilli(), Data: payload}
	if err = q.InsertSession(issued, now.UnixMilli()); err != nil {
		if errors.Is(err, store.ErrSessionCapacity) {
			return reject(fail(429, "too many agent sessions"))
		}
		return store.SessionRow{}, "", err
	}
	for _, key := range replaced {
		if err = revokeAgentSession(q, key, now.UnixMilli()); err != nil {
			return store.SessionRow{}, "", err
		}
	}
	return issued, resolved, nil
}
func sqlMember(q *store.Queries, pid, uid string) (store.ProjectRow, error) {
	p, err := q.Project(pid)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fail(404, "project not found")
	}
	if err != nil {
		return p, err
	}
	if _, err = q.User(uid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return p, fail(404, "project not found")
		}
		return p, err
	}
	if _, err = q.ProjectRole(pid, uid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return p, fail(404, "project not found")
		}
		return p, err
	}
	if p.Deleting {
		return p, fail(409, "project deletion is in progress")
	}
	return p, nil
}
