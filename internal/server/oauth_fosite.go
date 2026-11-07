package server

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"github.com/ory/fosite/handler/oauth2"
)

// Each provider is scoped to one DB.View or DB.Update snapshot. All Fosite
// operations for a token exchange run under one Update, including replay
// revocation. No in-memory session store or nested database transaction is used.
func (s *Server) connectorOAuth(st *model.State) (fosite.OAuth2Provider, *oauthStorage) {
	config := &fosite.Config{
		GlobalSecret:        s.Cipher.OAuthSecret(),
		AccessTokenLifespan: time.Hour, RefreshTokenLifespan: 30 * 24 * time.Hour,
		AuthorizeCodeLifespan: time.Minute,
		EnforcePKCE:           true, EnablePKCEPlainChallengeMethod: false,
		RefreshTokenScopes:     []string{connectorScope},
		ScopeStrategy:          fosite.ExactScopeStrategy,
		OmitRedirectScopeParam: true,
	}
	storage := &oauthStorage{state: st, resource: s.Config.BaseURL + "/mcp"}
	strategy := &hashedOAuthStrategy{CoreStrategy: compose.NewOAuth2HMACStrategy(config)}
	provider := compose.Compose(config, storage, strategy,
		compose.OAuth2AuthorizeExplicitFactory, compose.OAuth2RefreshTokenGrantFactory,
		compose.OAuth2PKCEFactory)
	return provider, storage
}

// Only the database lookup key changes: tokens and their cryptographic
// validation remain entirely Fosite's. Hashing the full token also lets MCP
// authenticate by hash lookup without persisting a bearer credential.
type hashedOAuthStrategy struct{ oauth2.CoreStrategy }

func (*hashedOAuthStrategy) AccessTokenSignature(_ context.Context, token string) string {
	return auth.Hash(token)
}
func (*hashedOAuthStrategy) RefreshTokenSignature(_ context.Context, token string) string {
	return auth.Hash(token)
}
func (*hashedOAuthStrategy) AuthorizeCodeSignature(_ context.Context, token string) string {
	return auth.Hash(token)
}
func (s *hashedOAuthStrategy) GenerateAccessToken(ctx context.Context, r fosite.Requester) (string, string, error) {
	token, _, err := s.CoreStrategy.GenerateAccessToken(ctx, r)
	return token, auth.Hash(token), err
}
func (s *hashedOAuthStrategy) GenerateRefreshToken(ctx context.Context, r fosite.Requester) (string, string, error) {
	token, _, err := s.CoreStrategy.GenerateRefreshToken(ctx, r)
	return token, auth.Hash(token), err
}
func (s *hashedOAuthStrategy) GenerateAuthorizeCode(ctx context.Context, r fosite.Requester) (string, string, error) {
	token, _, err := s.CoreStrategy.GenerateAuthorizeCode(ctx, r)
	return token, auth.Hash(token), err
}

type connectorSession struct {
	fosite.DefaultSession
	ProjectIDs []string `json:"project_ids"`
}

func (s *connectorSession) Clone() fosite.Session {
	c := *s
	c.ExpiresAt = maps.Clone(s.ExpiresAt)
	c.Extra = maps.Clone(s.Extra)
	c.ProjectIDs = append([]string(nil), s.ProjectIDs...)
	return &c
}

type oauthStorage struct {
	state    *model.State
	resource string
	replay   bool
}

var errOAuthCapacity = errors.New("OAuth storage capacity exceeded")

func (s *oauthStorage) GetClient(_ context.Context, id string) (fosite.Client, error) {
	c, ok := s.state.OAuthClients[id]
	if !ok {
		return nil, fosite.ErrNotFound
	}
	return &fosite.DefaultClient{ID: c.ID, RedirectURIs: c.RedirectURIs, Public: true,
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		Scopes: []string{connectorScope}, Audience: []string{s.resource}}, nil
}

// Only public clients using token_endpoint_auth_method=none are registered.
func (*oauthStorage) ClientAssertionJWTValid(context.Context, string) error {
	return fosite.ErrInvalidClient
}
func (*oauthStorage) SetClientAssertionJWT(context.Context, string, time.Time) error {
	return fosite.ErrInvalidClient
}

func oauthRequest(data json.RawMessage, userID string, projects []string) (*fosite.Request, error) {
	if len(data) == 0 || userID == "" || len(projects) == 0 {
		return nil, fosite.ErrNotFound
	}
	session := &connectorSession{}
	r := &fosite.Request{Client: &fosite.DefaultClient{}, Session: session}
	if err := json.Unmarshal(data, r); err != nil {
		return nil, err
	}
	// Selection and user metadata are authoritative for both MCP and Fosite.
	session.Subject = userID
	session.ProjectIDs = append([]string(nil), projects...)
	return r, nil
}
func (s *oauthStorage) CreateAuthorizeCodeSession(_ context.Context, key string, r fosite.Requester) error {
	if len(s.state.OAuthCodes) >= 10000 {
		return errOAuthCapacity
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	session := r.GetSession().(*connectorSession)
	s.state.OAuthCodes[key] = model.OAuthCode{UserID: session.Subject, ProjectIDs: session.ProjectIDs,
		Expires: session.GetExpiresAt(fosite.AuthorizeCode), Active: true, Request: data}
	return nil
}
func (s *oauthStorage) GetAuthorizeCodeSession(_ context.Context, key string, _ fosite.Session) (fosite.Requester, error) {
	c, ok := s.state.OAuthCodes[key]
	if !ok {
		return nil, fosite.ErrNotFound
	}
	r, err := oauthRequest(c.Request, c.UserID, c.ProjectIDs)
	if err != nil {
		return nil, err
	}
	if _, ok := s.state.Users[c.UserID]; !ok {
		return nil, fosite.ErrNotFound
	}
	if !c.Active {
		s.replay = true
		return r, fosite.ErrInvalidatedAuthorizeCode
	}
	return r, nil
}
func (s *oauthStorage) InvalidateAuthorizeCodeSession(_ context.Context, key string) error {
	c, ok := s.state.OAuthCodes[key]
	if !ok {
		return fosite.ErrNotFound
	}
	if !c.Active {
		return fosite.ErrInvalidatedAuthorizeCode
	}
	c.Active = false
	s.state.OAuthCodes[key] = c
	return nil
}
func (s *oauthStorage) CreatePKCERequestSession(_ context.Context, key string, r fosite.Requester) error {
	c, ok := s.state.OAuthCodes[key]
	if !ok {
		return fosite.ErrNotFound
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	c.PKCE = data
	s.state.OAuthCodes[key] = c
	return nil
}
func (s *oauthStorage) GetPKCERequestSession(_ context.Context, key string, _ fosite.Session) (fosite.Requester, error) {
	c, ok := s.state.OAuthCodes[key]
	if !ok {
		return nil, fosite.ErrNotFound
	}
	return oauthRequest(c.PKCE, c.UserID, c.ProjectIDs)
}
func (s *oauthStorage) DeletePKCERequestSession(_ context.Context, key string) error {
	c, ok := s.state.OAuthCodes[key]
	if !ok {
		return fosite.ErrNotFound
	}
	c.PKCE = nil
	s.state.OAuthCodes[key] = c
	return nil
}
func (s *oauthStorage) createToken(key, kind string, r fosite.Requester) error {
	if len(s.state.OAuthTokens) >= 20000 {
		return errOAuthCapacity
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	session := r.GetSession().(*connectorSession)
	tokenType := fosite.AccessToken
	if kind == "refresh" {
		tokenType = fosite.RefreshToken
	}
	s.state.OAuthTokens[key] = model.OAuthGrant{Kind: kind, ClientID: r.GetClient().GetID(), UserID: session.Subject,
		ProjectIDs: session.ProjectIDs, Expires: session.GetExpiresAt(tokenType), Active: true, RequestID: r.GetID(), Request: data}
	return nil
}
func (s *oauthStorage) getToken(key, kind string) (fosite.Requester, error) {
	g, ok := s.state.OAuthTokens[key]
	if !ok || g.Kind != kind {
		return nil, fosite.ErrNotFound
	}
	if _, ok := s.state.Users[g.UserID]; !ok {
		return nil, fosite.ErrNotFound
	}
	r, err := oauthRequest(g.Request, g.UserID, g.ProjectIDs)
	if err != nil {
		return nil, err
	}
	if !g.Active {
		if kind == "refresh" {
			s.replay = true
		}
		return r, fosite.ErrInactiveToken
	}
	tokenType := fosite.AccessToken
	if kind == "refresh" {
		tokenType = fosite.RefreshToken
	}
	r.Session.SetExpiresAt(tokenType, g.Expires)
	return r, nil
}
func (s *oauthStorage) CreateAccessTokenSession(_ context.Context, key string, r fosite.Requester) error {
	return s.createToken(key, "access", r)
}
func (s *oauthStorage) GetAccessTokenSession(_ context.Context, key string, _ fosite.Session) (fosite.Requester, error) {
	return s.getToken(key, "access")
}
func (s *oauthStorage) DeleteAccessTokenSession(_ context.Context, key string) error {
	delete(s.state.OAuthTokens, key)
	return nil
}
func (s *oauthStorage) CreateRefreshTokenSession(_ context.Context, key, _ string, r fosite.Requester) error {
	return s.createToken(key, "refresh", r)
}
func (s *oauthStorage) GetRefreshTokenSession(_ context.Context, key string, _ fosite.Session) (fosite.Requester, error) {
	return s.getToken(key, "refresh")
}
func (s *oauthStorage) DeleteRefreshTokenSession(_ context.Context, key string) error {
	g, ok := s.state.OAuthTokens[key]
	if ok {
		g.Active = false
		s.state.OAuthTokens[key] = g
	}
	return nil
}
func (s *oauthStorage) RevokeRefreshToken(_ context.Context, id string) error {
	// Retain inactive refresh tokens until expiry so reuse can revoke the family.
	for k, g := range s.state.OAuthTokens {
		if g.Kind == "refresh" && g.RequestID == id {
			g.Active = false
			s.state.OAuthTokens[k] = g
		}
	}
	return nil
}
func (s *oauthStorage) RevokeAccessToken(_ context.Context, id string) error {
	for k, g := range s.state.OAuthTokens {
		if g.Kind == "access" && g.RequestID == id {
			delete(s.state.OAuthTokens, k)
		}
	}
	return nil
}
func (s *oauthStorage) RotateRefreshToken(ctx context.Context, id, key string) error {
	g, ok := s.state.OAuthTokens[key]
	if !ok || !g.Active || g.RequestID != id {
		return fosite.ErrInactiveToken
	}
	if err := s.RevokeRefreshToken(ctx, id); err != nil {
		return err
	}
	return s.RevokeAccessToken(ctx, id)
}
