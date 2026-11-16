package store

import (
	"database/sql"
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
	Name      string `json:"name"`
	RevokedAt *int64 `json:"revoked_at"`
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

func (q *Queries) InsertSession(v SessionRow, now int64) error {
	var count int
	if err := q.row("SELECT count(*) FROM sessions WHERE kind=? AND expires>? AND NOT (kind='cli_login' AND status='active')", v.Kind, now).Scan(&count); err != nil {
		return err
	}
	if count >= 10000 {
		return ErrSessionCapacity
	}
	_, err := q.exec("INSERT INTO sessions("+sessionsColumns+") VALUES(?,?,?,?,?,?,?,?)", v.ID, v.Kind, v.Status, v.UserID, v.ProjectID, v.Expires, v.Created, string(v.Data))
	return err
}

var ErrSessionCapacity = errors.New("session storage capacity exceeded")

func (q *Queries) TakeLoginCode(id string, now int64) (SessionRow, error) {
	v, err := q.Session(id)
	if err != nil {
		return v, err
	}
	res, err := q.exec("DELETE FROM sessions WHERE id=? AND kind='login_code' AND status='pending' AND expires>?", id, now)
	if err != nil {
		return SessionRow{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return SessionRow{}, err
	}
	if n != 1 {
		return SessionRow{}, sql.ErrNoRows
	}
	return v, nil
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
func (q *Queries) PruneSessions(now int64) error {
	if _, err := q.exec("DELETE FROM sessions WHERE expires<=?", now); err != nil {
		return err
	}
	_, err := q.exec("DELETE FROM sessions WHERE kind='cli_login' AND status='active'")
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
