package server

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
func TestLoopbackMismatchConsumesCodeAfterCommit(t *testing.T) {
	d, _ := sqlAuthFixture(t)
	now := time.UnixMilli(1000)
	data := store.CLILoginData{LoopbackCodeHash: sqlTestPtr(auth.Hash("correct")), LoopbackChallenge: sqlTestPtr(auth.Challenge("verifier"))}
	sqlWrite(t, d, func(q *store.Queries) error {
		return q.InsertSession(store.SessionRow{ID: auth.Hash("device"), Kind: "cli_login", Status: "pending", UserID: "u", Expires: 2000, Data: sqlJSON(t, data)})
	})
	for _, code := range []string{"wrong", "correct"} {
		err := d.Write(t.Context(), func(q *store.Queries) error {
			_, err := exchangeCLILogin(q, "device", code, "verifier", "new-token", now)
			return err
		})
		expectSQLStatus(t, err, 410)
	}
	if err := d.Read(t.Context(), func(q *store.Queries) error {
		v, err := q.Session(auth.Hash("device"))
		if err == nil && v.Status != "active" {
			t.Fatal("consumption lost")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func TestLoopbackIssuanceFailureRollsBackConsumption(t *testing.T) {
	d, _ := sqlAuthFixture(t)
	now := time.UnixMilli(1000)
	data := store.CLILoginData{LoopbackCodeHash: sqlTestPtr(auth.Hash("code")), LoopbackChallenge: sqlTestPtr(auth.Challenge("verifier"))}
	sqlWrite(t, d, func(q *store.Queries) error {
		if err := q.InsertSession(store.SessionRow{ID: auth.Hash("device"), Kind: "cli_login", Status: "pending", UserID: "u", Expires: 2000, Data: sqlJSON(t, data)}); err != nil {
			return err
		}
		return q.InsertSession(store.SessionRow{ID: auth.Hash("collision"), Kind: "web", Status: "active", UserID: "u", Expires: 2000, Data: json.RawMessage(`{}`)})
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
func TestAgentLoginRejectionConsumesCode(t *testing.T) {
	d, _ := sqlAuthFixture(t)
	sqlWrite(t, d, func(q *store.Queries) error {
		return q.InsertSession(store.SessionRow{ID: auth.Hash("code"), Kind: "login_code", Status: "pending", UserID: "u", ProjectID: sqlTestPtr("p"), Expires: 2000, Data: sqlJSON(t, store.LoginCodeData{GrantKey: "g", Role: sqlTestPtr("read")})})
	})
	err := d.Write(t.Context(), func(q *store.Queries) error {
		_, _, err := exchangeAgentLogin(q, "code", " ", "", "a", "secret", time.UnixMilli(1000))
		return err
	})
	expectSQLStatus(t, err, 400)
	err = d.Write(t.Context(), func(q *store.Queries) error {
		_, _, err := exchangeAgentLogin(q, "code", "name", "", "a", "secret", time.UnixMilli(1000))
		return err
	})
	expectSQLStatus(t, err, 401)
}
func sqlOAuthRequest(now time.Time) *fosite.Request {
	session := &connectorSession{DefaultSession: fosite.DefaultSession{Subject: "u", ExpiresAt: map[fosite.TokenType]time.Time{fosite.AuthorizeCode: now.Add(time.Minute), fosite.AccessToken: now.Add(time.Hour), fosite.RefreshToken: now.Add(24 * time.Hour)}}, ProjectIDs: []string{"p"}}
	return &fosite.Request{ID: "family", RequestedAt: now, Client: &fosite.DefaultClient{ID: "client", Public: true, RedirectURIs: []string{"https://example.com/callback"}}, Session: session, Form: url.Values{"code_challenge": {"challenge"}, "code_challenge_method": {"S256"}}}
}
func TestConcurrentOAuthExchangeRevokesReplayFamily(t *testing.T) {
	for _, kind := range []string{"code", "refresh"} {
		t.Run(kind, func(t *testing.T) {
			d, c := sqlAuthFixture(t)
			now := time.Now().UTC().Truncate(time.Millisecond)
			r := sqlOAuthRequest(now)
			sqlWrite(t, d, func(q *store.Queries) error {
				_, s := newSQLOAuth(q, c, "https://example.com/mcp", now)
				if kind == "code" {
					return s.CreateAuthorizeCodeSession(t.Context(), "original", r)
				}
				return s.CreateRefreshTokenSession(t.Context(), "original", "", r)
			})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for range 2 {
				wg.Go(func() {
					results <- d.Write(t.Context(), func(q *store.Queries) error {
						_, s := newSQLOAuth(q, c, "https://example.com/mcp", now)
						var request fosite.Requester
						var err error
						if kind == "code" {
							request, err = s.GetAuthorizeCodeSession(t.Context(), "original", nil)
						} else {
							request, err = s.GetRefreshTokenSession(t.Context(), "original", nil)
						}
						if err != nil {
							if s.replay {
								return store.RejectAfterCommit(err)
							}
							return err
						}
						if kind == "code" {
							err = s.InvalidateAuthorizeCodeSession(t.Context(), "original")
						} else {
							err = s.RotateRefreshToken(t.Context(), "family", "original")
						}
						if err != nil {
							return err
						}
						if err = s.CreateAccessTokenSession(t.Context(), "new-access", request); err != nil {
							return err
						}
						return s.CreateRefreshTokenSession(t.Context(), "new-refresh", "", request)
					})
				})
			}
			wg.Wait()
			close(results)
			success := 0
			for err := range results {
				if err == nil {
					success++
				} else {
					var committed *store.CommittedRejection
					if !errors.As(err, &committed) {
						t.Fatal(err)
					}
				}
			}
			if success != 1 {
				t.Fatal(success)
			}
			if err := d.Read(t.Context(), func(q *store.Queries) error {
				if _, err := q.Session("new-access"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("replay did not revoke access", err)
				}
				v, err := q.Session("new-refresh")
				if err != nil {
					return err
				}
				var data store.MCPData
				if err = json.Unmarshal(v.Data, &data); err != nil {
					return err
				}
				if data.Active {
					t.Fatal("replay did not revoke refresh")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestOAuthOrdinaryRejectionRollsBackCode(t *testing.T) {
	d, c := sqlAuthFixture(t)
	now := time.Now().UTC()
	sqlWrite(t, d, func(q *store.Queries) error {
		_, s := newSQLOAuth(q, c, "resource", now)
		return s.CreateAuthorizeCodeSession(t.Context(), "code", sqlOAuthRequest(now))
	})
	rejected := errors.New("PKCE mismatch")
	err := d.Write(t.Context(), func(q *store.Queries) error {
		_, s := newSQLOAuth(q, c, "resource", now)
		if err := s.InvalidateAuthorizeCodeSession(t.Context(), "code"); err != nil {
			return err
		}
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	if err := d.Read(t.Context(), func(q *store.Queries) error {
		v, err := q.Session("code")
		if err == nil && v.Status != "pending" {
			t.Fatal("ordinary rejection consumed code")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func TestExpiredAccessSurvivesWhileLoginCodeReferencesIt(t *testing.T) {
	d, _ := sqlAuthFixture(t)
	sqlWrite(t, d, func(q *store.Queries) error {
		if err := q.InsertSession(store.SessionRow{ID: "access", Kind: "mcp", Status: "active", UserID: "u", Expires: 100, Data: sqlJSON(t, store.MCPData{TokenKind: "access", Active: true, ProjectIDs: []string{"p"}})}); err != nil {
			return err
		}
		if err := q.InsertSession(store.SessionRow{ID: "code", Kind: "login_code", Status: "pending", UserID: "u", Expires: 200, Data: sqlJSON(t, store.LoginCodeData{GrantKey: "access"})}); err != nil {
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
