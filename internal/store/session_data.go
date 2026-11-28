package store

import (
	"encoding/json"
	"errors"
	"log/slog"
)

type SessionRoleData struct {
	AgentID *string `json:"agent_id"`
	Role    *string `json:"role"`
}

type IdentitySessionData struct {
	IdentityProvider     *string `json:"identity_provider"`
	IdentityID           *string `json:"identity_id"`
	IdentityAccessToken  *string `json:"identity_access_token"`
	IdentityRefreshToken *string `json:"identity_refresh_token"`
	IdentityExpiresAt    *int64  `json:"identity_expires_at"`
}
type CLILoginData struct {
	IdentitySessionData
	LoopbackChallenge   *string `json:"loopback_challenge"`
	LoopbackRedirectURI *string `json:"loopback_redirect_uri"`
	LoopbackState       *string `json:"loopback_state"`
	LoopbackCodeHash    *string `json:"loopback_code_hash"`
}
type AgentSessionData struct {
	SessionRoleData
	Name string `json:"name"`
}
type MCPData struct {
	TokenKind  string          `json:"token_kind"`
	ClientID   string          `json:"client_id"`
	RequestID  string          `json:"request_id"`
	ProjectIDs []string        `json:"project_ids"`
	Request    json.RawMessage `json:"request"`
}
type OAuthCodeData struct {
	ProjectIDs []string        `json:"project_ids"`
	Request    json.RawMessage `json:"request"`
	PKCE       json.RawMessage `json:"pkce"`
}
type LoginCodeData struct {
	GrantKey string  `json:"grant_key"`
	Role     *string `json:"role"`
}

// Session payloads may contain encrypted credentials and PKCE material. Logs and
// accidental JSON responses must never serialize the raw stored row.
func (SessionRow) String() string               { return "[redacted session]" }
func (SessionRow) GoString() string             { return "[redacted session]" }
func (SessionRow) LogValue() slog.Value         { return slog.StringValue("[redacted session]") }
func (SessionRow) MarshalJSON() ([]byte, error) { return []byte(`"[redacted session]"`), nil }

// sessionLimits bounds the rows that one user can hold per session kind.
// InsertSession deletes the oldest rows beyond the limit, so that one user
// can never fill the table for everyone. Revoked refresh tokens go first.
var sessionLimits = map[string]int{"user": 50, "mcp": 1000, "oauth_code": 20, "login_code": 20}

func (q *Queries) InsertSession(v SessionRow) error {
	if _, err := q.exec("INSERT INTO sessions("+sessionsColumns+") VALUES(?,?,?,?,?,?,?,?)", v.ID, v.Kind, v.Status, v.UserID, v.ProjectID, v.Expires, v.Created, string(v.Data)); err != nil {
		return err
	}
	limit, ok := sessionLimits[v.Kind]
	if !ok {
		return nil
	}
	_, err := q.exec("DELETE FROM sessions WHERE id IN (SELECT id FROM sessions WHERE user_id=? AND kind=? ORDER BY status='revoked', created DESC, id DESC LIMIT -1 OFFSET ?)", v.UserID, v.Kind, limit)
	return err
}

// TakeSession consumes a single-use pending session by deleting it.
func (q *Queries) TakeSession(id, kind string, now int64) (SessionRow, error) {
	if kind != "cli_login" && kind != "login_code" && kind != "signup" {
		return SessionRow{}, errors.New("session kind is not single-use")
	}
	return scanSession(q.row("DELETE FROM sessions WHERE id=? AND kind=? AND expires>? RETURNING "+sessionsColumns, id, kind, now))
}
func (q *Queries) RevokeOAuthFamily(user, id string) error {
	rows, err := q.UserSessions(user, "mcp")
	if err != nil {
		return err
	}
	for _, v := range rows {
		var data MCPData
		if err := json.Unmarshal(v.Data, &data); err != nil {
			return err
		}
		if data.RequestID != id {
			continue
		}
		if data.TokenKind == "access" {
			if err := q.DeleteSession(v.ID); err != nil {
				return err
			}
		} else {
			v.Status = "revoked"
			if err := q.PutSession(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// revokedRetention keeps a revoked refresh token long enough to detect its
// replay and revoke the family; PruneSessions deletes it afterwards.
const revokedRetention = 7 * 24 * 60 * 60 * 1000

// clientRetention is how long a registered OAuth client may stay unused
// before PruneSessions deletes it.
const clientRetention = 24 * 60 * 60 * 1000

func (q *Queries) PruneSessions(now int64) error {
	if _, err := q.exec("DELETE FROM sessions WHERE expires<=? OR (kind='mcp' AND status='revoked' AND created<=?)", now, now-revokedRetention); err != nil {
		return err
	}
	_, err := q.exec("DELETE FROM oauth_clients WHERE json_extract(data,'$.created')<=? AND client_id NOT IN (SELECT json_extract(data,'$.client_id') FROM sessions WHERE kind='mcp' AND json_extract(data,'$.client_id') IS NOT NULL)", now-clientRetention)
	return err
}

func (d CLILoginData) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		IdentityProvider     *string `json:"identity_provider"`
		IdentityID           *string `json:"identity_id"`
		IdentityAccessToken  *string `json:"identity_access_token"`
		IdentityRefreshToken *string `json:"identity_refresh_token"`
		IdentityExpiresAt    *int64  `json:"identity_expires_at"`
		LoopbackChallenge    *string `json:"loopback_challenge"`
		LoopbackRedirectURI  *string `json:"loopback_redirect_uri"`
		LoopbackState        *string `json:"loopback_state"`
		LoopbackCodeHash     *string `json:"loopback_code_hash"`
	}{d.IdentityProvider, d.IdentityID, d.IdentityAccessToken, d.IdentityRefreshToken, d.IdentityExpiresAt, d.LoopbackChallenge, d.LoopbackRedirectURI, d.LoopbackState, d.LoopbackCodeHash})
}

func SessionData[T any](row SessionRow) (T, error) {
	var data T
	err := unmarshalData(string(row.Data), &data)
	return data, err
}
func EncodeSessionData[T any](data T) (json.RawMessage, error) {
	raw, err := marshalData(data)
	return json.RawMessage(raw), err
}
