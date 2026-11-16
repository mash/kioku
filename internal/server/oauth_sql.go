package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/store"
	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
)

// oauthSQL binds every Fosite operation in an exchange to one writer transaction.
// Only replay rejection sets replay, allowing the caller to commit revocation.
type oauthSQL struct {
	q          *store.Queries
	resource   string
	now        time.Time
	replay     bool
	familyUser string
	failure    error
}

func newSQLOAuth(q *store.Queries, cipher *auth.Cipher, resource string, now time.Time) (fosite.OAuth2Provider, *oauthSQL) {
	config := &fosite.Config{GlobalSecret: cipher.OAuthSecret(), AccessTokenLifespan: time.Hour, RefreshTokenLifespan: 30 * 24 * time.Hour, AuthorizeCodeLifespan: time.Minute, EnforcePKCE: true, EnablePKCEPlainChallengeMethod: false, RefreshTokenScopes: []string{connectorScope}, ScopeStrategy: fosite.ExactScopeStrategy, OmitRedirectScopeParam: true}
	s := &oauthSQL{q: q, resource: resource, now: now}
	strategy := &hashedOAuthStrategy{CoreStrategy: compose.NewOAuth2HMACStrategy(config)}
	return compose.Compose(config, s, strategy, compose.OAuth2AuthorizeExplicitFactory, compose.OAuth2RefreshTokenGrantFactory, compose.OAuth2PKCEFactory), s
}

func encodeSQLRequest(r fosite.Requester) (json.RawMessage, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var v map[string]json.RawMessage
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	for _, key := range []string{"scopes", "grantedScopes", "requestedAudience", "grantedAudience"} {
		if len(v[key]) == 0 || string(v[key]) == "null" {
			v[key] = json.RawMessage(`[]`)
		}
	}
	var client map[string]json.RawMessage
	if err = json.Unmarshal(v["client"], &client); err != nil {
		return nil, err
	}
	for _, key := range []string{"redirect_uris", "grant_types", "response_types", "scopes", "audience"} {
		if len(client[key]) == 0 || string(client[key]) == "null" {
			client[key] = json.RawMessage(`[]`)
		}
	}
	v["client"], err = json.Marshal(client)
	if err != nil {
		return nil, err
	}
	var requested time.Time
	if err = json.Unmarshal(v["requestedAt"], &requested); err != nil {
		return nil, err
	}
	v["requestedAt"], err = json.Marshal(requested.UnixMilli())
	if err != nil {
		return nil, err
	}
	var session map[string]json.RawMessage
	if err = json.Unmarshal(v["session"], &session); err != nil {
		return nil, err
	}
	var expiries map[string]time.Time
	if err = json.Unmarshal(session["expires_at"], &expiries); err != nil {
		return nil, err
	}
	stored := map[string]*int64{}
	for _, kind := range []string{"authorize_code", "access_token", "refresh_token"} {
		var ms *int64
		if t := expiries[kind]; !t.IsZero() {
			n := t.UnixMilli()
			ms = &n
		}
		stored[kind] = ms
	}
	session["expires_at"], err = json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	delete(session, "project_ids")
	v["session"], err = json.Marshal(session)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
func decodeSQLRequest(raw json.RawMessage, user string, projects []string) (*fosite.Request, error) {
	var v map[string]json.RawMessage
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	var requested int64
	if err := json.Unmarshal(v["requestedAt"], &requested); err != nil {
		return nil, err
	}
	var err error
	v["requestedAt"], err = json.Marshal(time.UnixMilli(requested).UTC())
	if err != nil {
		return nil, err
	}
	var session map[string]json.RawMessage
	if err = json.Unmarshal(v["session"], &session); err != nil {
		return nil, err
	}
	var expiries map[string]*int64
	if err = json.Unmarshal(session["expires_at"], &expiries); err != nil {
		return nil, err
	}
	restored := map[string]time.Time{}
	for _, kind := range []string{"authorize_code", "access_token", "refresh_token"} {
		t := time.Time{}
		if ms := expiries[kind]; ms != nil {
			t = time.UnixMilli(*ms).UTC()
		}
		restored[kind] = t
	}
	session["expires_at"], err = json.Marshal(restored)
	if err != nil {
		return nil, err
	}
	v["session"], err = json.Marshal(session)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return oauthRequest(b, user, projects)
}
func (s *oauthSQL) GetClient(_ context.Context, id string) (fosite.Client, error) {
	c, err := s.q.OAuthClient(id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, s.storageError(err)
	}
	return &fosite.DefaultClient{ID: id, RedirectURIs: c.Data.RedirectURIs, Public: true, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, Scopes: []string{connectorScope}, Audience: []string{s.resource}}, nil
}
func (*oauthSQL) ClientAssertionJWTValid(context.Context, string) error {
	return fosite.ErrInvalidClient
}
func (*oauthSQL) SetClientAssertionJWT(context.Context, string, time.Time) error {
	return fosite.ErrInvalidClient
}
func (s *oauthSQL) read(id, kind string) (store.SessionRow, error) {
	v, err := s.q.Session(id)
	if errors.Is(err, sql.ErrNoRows) {
		return v, fosite.ErrNotFound
	}
	if err != nil {
		return v, s.storageError(err)
	}
	if v.Kind != kind || s.now.UnixMilli() >= v.Expires {
		return store.SessionRow{}, fosite.ErrNotFound
	}
	if _, err := s.q.User(v.UserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.SessionRow{}, fosite.ErrNotFound
		}
		return store.SessionRow{}, s.storageError(err)
	}
	s.familyUser = v.UserID
	return v, nil
}
func (s *oauthSQL) CreateAuthorizeCodeSession(_ context.Context, key string, r fosite.Requester) error {
	request, err := encodeSQLRequest(r)
	if err != nil {
		return s.storageError(err)
	}
	session := r.GetSession().(*connectorSession)
	data, err := store.EncodeSessionData(store.OAuthCodeData{ProjectIDs: session.ProjectIDs, Request: request})
	if err != nil {
		return s.storageError(err)
	}
	return s.storageError(s.q.InsertSession(store.SessionRow{ID: key, Kind: "oauth_code", Status: "pending", UserID: session.Subject, Expires: session.GetExpiresAt(fosite.AuthorizeCode).UnixMilli(), Created: r.GetRequestedAt().UnixMilli(), Data: data}))
}
func (s *oauthSQL) GetAuthorizeCodeSession(_ context.Context, key string, _ fosite.Session) (fosite.Requester, error) {
	v, err := s.read(key, "oauth_code")
	if err != nil {
		return nil, s.storageError(err)
	}
	data, err := store.SessionData[store.OAuthCodeData](v)
	if err != nil {
		return nil, s.storageError(err)
	}
	r, err := decodeSQLRequest(data.Request, v.UserID, data.ProjectIDs)
	if err != nil {
		return nil, s.storageError(err)
	}
	if v.Status != "pending" {
		if err = s.q.RevokeOAuthFamily(v.UserID, r.ID); err != nil {
			return nil, s.storageError(err)
		}
		s.replay = true
		return r, fosite.ErrInvalidatedAuthorizeCode
	}
	return r, nil
}
func (s *oauthSQL) InvalidateAuthorizeCodeSession(_ context.Context, key string) error {
	ok, err := s.q.ConsumeSession(key, "oauth_code", s.now.UnixMilli())
	if err != nil {
		return s.storageError(err)
	}
	if !ok {
		return fosite.ErrInvalidatedAuthorizeCode
	}
	return nil
}
func (s *oauthSQL) CreatePKCERequestSession(_ context.Context, key string, r fosite.Requester) error {
	v, err := s.read(key, "oauth_code")
	if err != nil {
		return s.storageError(err)
	}
	data, err := store.SessionData[store.OAuthCodeData](v)
	if err != nil {
		return s.storageError(err)
	}
	data.PKCE, err = encodeSQLRequest(r)
	if err != nil {
		return s.storageError(err)
	}
	v.Data, err = store.EncodeSessionData(data)
	if err != nil {
		return s.storageError(err)
	}
	return s.storageError(s.q.PutSession(v))
}
func (s *oauthSQL) GetPKCERequestSession(_ context.Context, key string, _ fosite.Session) (fosite.Requester, error) {
	v, err := s.read(key, "oauth_code")
	if err != nil {
		return nil, s.storageError(err)
	}
	data, err := store.SessionData[store.OAuthCodeData](v)
	if err != nil {
		return nil, s.storageError(err)
	}
	return decodeSQLRequest(data.PKCE, v.UserID, data.ProjectIDs)
}
func (s *oauthSQL) DeletePKCERequestSession(_ context.Context, key string) error {
	v, err := s.read(key, "oauth_code")
	if err != nil {
		return s.storageError(err)
	}
	data, err := store.SessionData[store.OAuthCodeData](v)
	if err != nil {
		return s.storageError(err)
	}
	data.PKCE = nil
	v.Data, err = store.EncodeSessionData(data)
	if err != nil {
		return s.storageError(err)
	}
	return s.storageError(s.q.PutSession(v))
}
func (s *oauthSQL) createToken(key, kind string, r fosite.Requester) error {
	request, err := encodeSQLRequest(r)
	if err != nil {
		return s.storageError(err)
	}
	session := r.GetSession().(*connectorSession)
	tokenType := fosite.AccessToken
	if kind == "refresh" {
		tokenType = fosite.RefreshToken
	}
	data, err := store.EncodeSessionData(store.MCPData{TokenKind: kind, Active: true, ClientID: r.GetClient().GetID(), RequestID: r.GetID(), ProjectIDs: session.ProjectIDs, Request: request})
	if err != nil {
		return s.storageError(err)
	}
	return s.storageError(s.q.InsertSession(store.SessionRow{ID: key, Kind: "mcp", Status: "active", UserID: session.Subject, Expires: session.GetExpiresAt(tokenType).UnixMilli(), Created: r.GetRequestedAt().UnixMilli(), Data: data}))
}
func (s *oauthSQL) getToken(key, kind string) (fosite.Requester, error) {
	v, err := s.read(key, "mcp")
	if err != nil {
		return nil, s.storageError(err)
	}
	data, err := store.SessionData[store.MCPData](v)
	if err != nil {
		return nil, s.storageError(err)
	}
	if data.TokenKind != kind {
		return nil, fosite.ErrNotFound
	}
	r, err := decodeSQLRequest(data.Request, v.UserID, data.ProjectIDs)
	if err != nil {
		return nil, s.storageError(err)
	}
	if !data.Active {
		if kind == "refresh" {
			if err = s.q.RevokeOAuthFamily(v.UserID, data.RequestID); err != nil {
				return nil, s.storageError(err)
			}
			s.replay = true
		}
		return r, fosite.ErrInactiveToken
	}
	tokenType := fosite.AccessToken
	if kind == "refresh" {
		tokenType = fosite.RefreshToken
	}
	r.Session.SetExpiresAt(tokenType, time.UnixMilli(v.Expires).UTC())
	return r, nil
}
func (s *oauthSQL) CreateAccessTokenSession(_ context.Context, key string, r fosite.Requester) error {
	return s.createToken(key, "access", r)
}
func (s *oauthSQL) GetAccessTokenSession(_ context.Context, key string, _ fosite.Session) (fosite.Requester, error) {
	return s.getToken(key, "access")
}
func (s *oauthSQL) DeleteAccessTokenSession(_ context.Context, key string) error {
	return s.storageError(s.q.DeleteSession(key))
}
func (s *oauthSQL) CreateRefreshTokenSession(_ context.Context, key, _ string, r fosite.Requester) error {
	return s.createToken(key, "refresh", r)
}
func (s *oauthSQL) GetRefreshTokenSession(_ context.Context, key string, _ fosite.Session) (fosite.Requester, error) {
	return s.getToken(key, "refresh")
}
func (s *oauthSQL) DeleteRefreshTokenSession(_ context.Context, key string) error {
	v, err := s.read(key, "mcp")
	if err != nil {
		return s.storageError(err)
	}
	data, err := store.SessionData[store.MCPData](v)
	if err != nil {
		return s.storageError(err)
	}
	data.Active = false
	v.Data, err = store.EncodeSessionData(data)
	if err != nil {
		return s.storageError(err)
	}
	return s.storageError(s.q.PutSession(v))
}
func (s *oauthSQL) RevokeRefreshToken(_ context.Context, id string) error {
	return s.storageError(s.q.RevokeOAuthFamily(s.familyUser, id))
}
func (s *oauthSQL) RevokeAccessToken(_ context.Context, id string) error {
	return s.storageError(s.q.RevokeOAuthFamily(s.familyUser, id))
}
func (s *oauthSQL) RotateRefreshToken(_ context.Context, id, key string) error {
	v, err := s.read(key, "mcp")
	if err != nil {
		return s.storageError(err)
	}
	data, err := store.SessionData[store.MCPData](v)
	if err != nil {
		return s.storageError(err)
	}
	if data.TokenKind != "refresh" || !data.Active || data.RequestID != id {
		return fosite.ErrInactiveToken
	}
	return s.storageError(s.q.RevokeOAuthFamily(v.UserID, id))
}

func (s *oauthSQL) storageError(err error) error {
	var protocol *fosite.RFC6749Error
	if err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, store.ErrSessionCapacity) && !errors.As(err, &protocol) {
		s.failure = err
	}
	return err
}
