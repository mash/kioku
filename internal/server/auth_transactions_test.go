package server

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
	"github.com/ory/fosite"
)

func sqlAuthFixture(t *testing.T) (*store.Database, *auth.Cipher) {
	t.Helper()
	d, err := store.OpenDatabase(filepath.Join(t.TempDir(), "catalog.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Write(t.Context(), func(q *store.Queries) error {
		if err := q.PutUser(store.UserRow{ID: "u", Email: "u@example.com"}); err != nil {
			return err
		}
		if err := q.PutOrganization(store.OrganizationRow{ID: "o", Slug: "org"}); err != nil {
			return err
		}
		if err := q.PutProject(store.ProjectRow{ID: "p", OrgID: "o", Slug: "project"}); err != nil {
			return err
		}
		return q.PutProjectMember("p", "u", "owner")
	}); err != nil {
		t.Fatal(err)
	}
	return d, c
}
func sqlTestPtr[T any](v T) *T { return &v }
func sqlJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func sqlWrite(t *testing.T, d catalog, fn func(*store.Queries) error) {
	t.Helper()
	if err := d.Write(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}
func expectSQLStatus(t *testing.T, err error, status int) {
	t.Helper()
	var api apiError
	if !errors.As(err, &api) || api.status != status {
		t.Fatalf("status %d: %v", status, err)
	}
}
func TestLoopbackIssuanceFailureRollsBackConsumption(t *testing.T) {
	t.Parallel()
	d, _ := sqlAuthFixture(t)
	now := time.UnixMilli(1000)
	data := store.CLILoginData{LoopbackCodeHash: sqlTestPtr(auth.Hash("code")), LoopbackChallenge: sqlTestPtr(auth.Challenge("verifier"))}
	sqlWrite(t, d, func(q *store.Queries) error {
		if err := q.InsertSession(store.SessionRow{ID: auth.Hash("device"), Kind: "cli_login", Status: "pending", UserID: "u", Expires: 2000, Data: sqlJSON(t, data)}, 0); err != nil {
			return err
		}
		return q.InsertSession(store.SessionRow{ID: auth.Hash("collision"), Kind: "web", Status: "active", UserID: "u", Expires: 2000, Data: json.RawMessage(`{}`)}, 0)
	})
	err := d.Write(t.Context(), func(q *store.Queries) error {
		_, err := exchangeCLILogin(q, "device", "code", "verifier", "collision", now)
		return err
	})
	if err == nil {
		t.Fatal("collision accepted")
	}
	sqlWrite(t, d, func(q *store.Queries) error {
		v, err := q.Session(auth.Hash("device"))
		if err != nil {
			return err
		}
		if v.Status != "pending" {
			t.Fatal("failed issuance consumed code")
		}
		_, err = exchangeCLILogin(q, "device", "code", "verifier", "unique", now)
		return err
	})
}
func TestAgentLoginCapacityRejectionKeepsExistingCredential(t *testing.T) {
	t.Parallel()
	d, _ := sqlAuthFixture(t)
	now := time.UnixMilli(1000)
	old := store.SessionRow{ID: auth.Hash("old-agent"), Kind: "agent", Status: "active", UserID: "u", ProjectID: sqlTestPtr("p"), Expires: 2000, Data: sqlJSON(t, store.AgentSessionData{SessionRoleData: store.SessionRoleData{AgentID: sqlTestPtr("existing"), Role: sqlTestPtr("read")}, Name: "connector:host"})}
	code := store.SessionRow{ID: auth.Hash("code"), Kind: "login_code", Status: "pending", UserID: "u", ProjectID: sqlTestPtr("p"), Expires: 2000, Data: sqlJSON(t, store.LoginCodeData{GrantKey: "grant", Role: sqlTestPtr("read")})}
	sqlWrite(t, d, func(q *store.Queries) error {
		if err := q.PutSession(old); err != nil {
			return err
		}
		if err := q.PutSession(store.SessionRow{ID: "grant", Kind: "mcp", Status: "active", UserID: "u", Expires: 2000, Data: sqlJSON(t, store.MCPData{TokenKind: "access", ProjectIDs: []string{"p"}})}); err != nil {
			return err
		}
		// Fill the kind-wide limit without hitting this project's active limit.
		for i := range 9999 {
			row := old
			row.ID = "capacity-" + strconv.Itoa(i)
			row.ProjectID = sqlTestPtr("other-project")
			if err := q.PutSession(row); err != nil {
				return err
			}
		}
		return q.PutSession(code)
	})
	err := d.Write(t.Context(), func(q *store.Queries) error {
		_, _, err := exchangeAgentLogin(q, "code", "host", "", "new", "secret", now)
		return err
	})
	expectSQLStatus(t, err, 429)
	fixtureCheck(t, d.Read(t.Context(), func(q *store.Queries) error {
		if _, err := q.Session(code.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("rejected login did not consume its code: %v", err)
		}
		row, err := q.Session(old.ID)
		if err != nil {
			return err
		}
		var data store.AgentSessionData
		if err := json.Unmarshal(row.Data, &data); err != nil {
			return err
		}
		if data.RevokedAt != nil {
			t.Fatal("capacity rejection revoked the credential it failed to replace")
		}
		return nil
	}))
	// Once capacity is available, a fresh code issues the replacement and
	// revokes the old credential together.
	sqlWrite(t, d, func(q *store.Queries) error {
		if err := q.DeleteSession("capacity-0"); err != nil {
			return err
		}
		return q.PutSession(code)
	})
	sqlWrite(t, d, func(q *store.Queries) error {
		_, _, err := exchangeAgentLogin(q, "code", "host", "", "new", "secret", now)
		return err
	})
	fixtureCheck(t, d.Read(t.Context(), func(q *store.Queries) error {
		row, err := q.Session(old.ID)
		if err != nil {
			return err
		}
		var data store.AgentSessionData
		if err := json.Unmarshal(row.Data, &data); err != nil {
			return err
		}
		if data.RevokedAt == nil || *data.RevokedAt != now.UnixMilli() {
			t.Fatal("successful replacement did not revoke the old credential")
		}
		_, err = q.Session(auth.Hash("kioku_agent.new.secret"))
		return err
	}))
}

func sqlOAuthRequest(now time.Time) *fosite.Request {
	session := &connectorSession{DefaultSession: fosite.DefaultSession{Subject: "u", ExpiresAt: map[fosite.TokenType]time.Time{fosite.AuthorizeCode: now.Add(time.Minute), fosite.AccessToken: now.Add(time.Hour), fosite.RefreshToken: now.Add(24 * time.Hour)}}, ProjectIDs: []string{"p"}}
	return &fosite.Request{ID: "family", RequestedAt: now, Client: &fosite.DefaultClient{ID: "client", Public: true, RedirectURIs: []string{"https://example.com/callback"}}, Session: session, Form: url.Values{"code_challenge": {"challenge"}, "code_challenge_method": {"S256"}}}
}
func TestExpiredAccessSurvivesWhileLoginCodeReferencesIt(t *testing.T) {
	t.Parallel()
	d, _ := sqlAuthFixture(t)
	sqlWrite(t, d, func(q *store.Queries) error {
		if err := q.InsertSession(store.SessionRow{ID: "access", Kind: "mcp", Status: "active", UserID: "u", Expires: 100, Data: sqlJSON(t, store.MCPData{TokenKind: "access", ProjectIDs: []string{"p"}})}, 0); err != nil {
			return err
		}
		if err := q.InsertSession(store.SessionRow{ID: "code", Kind: "login_code", Status: "pending", UserID: "u", Expires: 200, Data: sqlJSON(t, store.LoginCodeData{GrantKey: "access"})}, 0); err != nil {
			return err
		}
		if err := q.PruneSessions(100); err != nil {
			return err
		}
		_, err := q.Session("access")
		return err
	})
	sqlWrite(t, d, func(q *store.Queries) error {
		if err := q.PruneSessions(200); err != nil {
			return err
		}
		if _, err := q.Session("access"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("expired retained", err)
		}
		return nil
	})
}
func TestSQLFositeRequestUsesMillisecondsAndParentSelection(t *testing.T) {
	t.Parallel()
	r := sqlOAuthRequest(time.Now().UTC())
	r.Session.(*connectorSession).Extra = map[string]any{"nested": map[string]any{"value": true}}
	b, err := encodeSQLRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "project_ids") || strings.Contains(string(b), "0001-01-01") {
		t.Fatal("redundant or old timestamp persisted")
	}
	for _, key := range []string{"scopes", "grantedScopes", "requestedAudience", "grantedAudience"} {
		if !strings.Contains(string(b), `"`+key+`":[]`) {
			t.Fatalf("empty array missing: %s", key)
		}
	}
	out, err := decodeSQLRequest(b, "other", []string{"another"})
	if err != nil {
		t.Fatal(err)
	}
	s := out.Session.(*connectorSession)
	if s.Subject != "other" || !reflect.DeepEqual(s.ProjectIDs, []string{"another"}) || s.Extra["nested"] == nil {
		t.Fatal("parent selection or extra lost")
	}
	if out.RequestedAt.UnixMilli() != r.RequestedAt.UnixMilli() {
		t.Fatal("time changed")
	}
}
