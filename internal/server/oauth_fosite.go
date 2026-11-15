package server

import (
	"context"
	"encoding/json"
	"maps"

	"github.com/mash/kioku/internal/auth"
	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/oauth2"
)

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
