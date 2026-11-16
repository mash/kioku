package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
)

type authenticatedSession struct {
	UserID, ProjectID, AgentID, JobID, Role string
	Expires                                 time.Time
	Identity                                store.IdentitySessionData
}

func requestBearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}
func (s *Server) session(r *http.Request) (string, authenticatedSession, error) {
	if identity, ok := r.Context().Value(authContextKey{}).(requestIdentity); ok {
		return identity.key, identity.principal, nil
	}
	token := requestBearer(r)
	if token == "" {
		if cookie, e := r.Cookie("kioku_session"); e == nil {
			token = cookie.Value
		}
	}
	key := auth.Hash(token)
	var principal authenticatedSession
	err := s.Store.Read(r.Context(), func(q *store.Queries) error {
		row, e := q.Session(key)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		now := time.Now()
		if e == nil && row.Active(now.UnixMilli()) {
			if _, e = q.User(row.UserID); errors.Is(e, sql.ErrNoRows) {
				return nil
			} else if e != nil {
				return e
			}
			switch row.Kind {
			case "web", "cli":
				identity, e := store.SessionData[store.IdentitySessionData](row)
				if e != nil {
					return e
				}
				principal = authenticatedSession{UserID: row.UserID, ProjectID: store.ValueString(row.ProjectID), AgentID: store.ValueString(identity.AgentID), Role: store.ValueString(identity.Role), Expires: storageDate(row.Expires), Identity: identity}
				return nil
			case "agent":
				if r.Header.Get("Authorization") == "" {
					return nil
				}
				agent, e := store.SessionData[store.AgentSessionData](row)
				if e != nil {
					return e
				}
				if agent.RevokedAt != nil {
					return nil
				}
				if _, e = sqlMember(q, store.ValueString(row.ProjectID), row.UserID); e != nil {
					var api apiError
					if errors.As(e, &api) {
						return nil
					}
					return e
				}
				principal = authenticatedSession{UserID: row.UserID, ProjectID: store.ValueString(row.ProjectID), AgentID: store.ValueString(agent.AgentID), Role: store.ValueString(agent.Role), Expires: storageDate(row.Expires)}
				return nil
			}
		}
		if r.Header.Get("Authorization") != "" && strings.HasPrefix(token, "kioku_agent.") {
			payload, e := s.Cipher.OpenJobCredential(token, now.UnixMilli())
			if e != nil {
				return nil
			}
			j, e := q.Job(payload.JobID)
			if errors.Is(e, sql.ErrNoRows) {
				return nil
			}
			if e != nil {
				return e
			}
			if !j.Running() || j.Data.Run.AbortReason != nil {
				return fail(403, "invalid job credential")
			}
			principal = authenticatedSession{UserID: j.Data.UserID, ProjectID: j.ProjectID, AgentID: payload.JobID, JobID: payload.JobID, Role: "job", Expires: storageDate(payload.Expires)}
		}
		return nil
	})
	return key, principal, err
}
func (s *Server) userID(r *http.Request) string {
	_, principal, _ := s.session(r)
	return principal.UserID
}
