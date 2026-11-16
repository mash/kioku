package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/ory/fosite"
)

func (p *migrationPlan) readRecords(ctx context.Context, records []legacyV2Record, objects blob.Store, cipher *auth.Cipher, now time.Time) error {
	for _, r := range records {
		if r.id == "" || decodeLegacyJSON(r.body, &map[string]json.RawMessage{}) != nil {
			return errors.New("invalid legacy catalog record")
		}
		switch r.kind {
		case "organizations":
			var v legacyV2Organization
			if e := decodeLegacyJSON(r.body, &v); e != nil {
				return e
			}
			if v.ID != r.id {
				return errors.New("organization identity mismatch")
			}
			if v.Members == nil {
				return errors.New("missing legacy organization membership")
			}
			p.organizations = append(p.organizations, OrganizationRow{ID: v.ID, Slug: v.Slug, Data: OrganizationData{Name: v.Name, Created: migrationCreated(v.Created)}})
			for user, role := range v.Members {
				switch role {
				case "owner":
					p.organizationOwners = append(p.organizationOwners, [2]string{v.ID, user})
				case "member":
					// Organization members have no rights; project membership grants access.
				default:
					return errors.New("invalid legacy organization role")
				}
			}
		case "users":
			var v legacyV2User
			if e := decodeLegacyJSON(r.body, &v); e != nil {
				return e
			}
			if v.ID != r.id {
				return errors.New("user identity mismatch")
			}
			p.users = append(p.users, UserRow{ID: v.ID, Email: v.Email, Data: UserData{Name: v.Name}})
			for provider, id := range v.Identities {
				p.identities = append(p.identities, [3]string{v.ID, provider, id})
			}
		case "projects":
			var v legacyV2Project
			if e := decodeLegacyJSON(r.body, &v); e != nil {
				return e
			}
			if v.ID != r.id || !validID(v.ID) {
				return errors.New("project identity mismatch")
			}
			if v.Members == nil {
				return errors.New("missing legacy project membership")
			}
			if v.Deleting {
				return errors.New("finish deleting projects with v2 before migration")
			}
			if v.StorageRoot != "" && v.StorageRoot != "projects/"+v.ID+"/" {
				return errors.New("unsupported project storage root")
			}
			if validator, ok := objects.(interface{ ValidateLegacyBucket(string) error }); ok {
				if err := validator.ValidateLegacyBucket(v.StorageBucket); err != nil {
					return err
				}
			}
			if e := p.readProject(ctx, v, objects, now); e != nil {
				return fmt.Errorf("project %s migration: %w", v.ID, e)
			}
		case "invites":
			var v legacyV2Invite
			if e := decodeLegacyJSON(r.body, &v); e != nil {
				return e
			}
			if v.ID != r.id {
				return errors.New("invite identity mismatch")
			}
			p.invites = append(p.invites, InviteRow{ID: v.ID, ProjectID: v.ProjectID, CreatedBy: v.CreatedBy, Expires: migrationCreated(v.Expires), UsedBy: NullableString(v.UsedBy), TokenHash: v.TokenHash, Data: InviteData{Email: NullableString(v.Email)}})
		case "oauth_clients":
			var v legacyV2OAuthClient
			if e := decodeLegacyJSON(r.body, &v); e != nil {
				return e
			}
			if v.ID != r.id {
				return errors.New("OAuth client identity mismatch")
			}
			p.clients = append(p.clients, OAuthClientRow{ClientID: v.ID, Data: OAuthClientData{ClientName: v.Name, Created: migrationCreated(v.Created), RedirectURIs: v.RedirectURIs}})
		case "sessions":
			var v legacyV2Session
			if e := decodeLegacyJSON(r.body, &v); e != nil {
				return e
			}
			// Agent-scoped legacy sessions are discarded like job sessions:
			// a plain user session would widen their access.
			if v.JobID != "" || v.Role == "job" || v.AgentID != "" {
				p.discardedRecords++
				continue
			}
			data, e := migrateIdentity(cipher, v.UserID, v.IdentityProvider, v.IdentityID, v.IdentityCredential)
			if e != nil {
				return e
			}
			if e = p.addSession(SessionRow{ID: r.id, Kind: "user", Status: "active", UserID: v.UserID, ProjectID: NullableString(v.ProjectID), Expires: migrationCreated(v.Expires), Created: migrationCreated(v.Created)}, data); e != nil {
				return e
			}
		case "agent_tokens":
			var v legacyV2AgentToken
			if e := decodeLegacyJSON(r.body, &v); e != nil {
				return e
			}
			if v.ID != r.id {
				return errors.New("agent identity mismatch")
			}
			if v.JobID != "" || v.Role == "job" || !v.RevokedAt.IsZero() {
				p.discardedRecords++
				continue
			}
			if e := p.addSession(SessionRow{ID: v.TokenHash, Kind: "agent", Status: "active", UserID: v.UserID, ProjectID: NullableString(v.ProjectID), Expires: migrationCreated(v.Expires), Created: migrationCreated(v.Created)}, AgentSessionData{SessionRoleData: SessionRoleData{AgentID: NullableString(v.ID), Role: NullableString(v.Role)}, Name: v.Name}); e != nil {
				return e
			}
		case "devices":
			var v legacyV2Device
			if e := decodeLegacyJSON(r.body, &v); e != nil {
				return e
			}
			if v.Used {
				p.discardedRecords++
				continue
			}
			identity, e := migrateIdentity(cipher, v.UserID, v.IdentityProvider, v.IdentityID, v.IdentityCredential)
			if e != nil {
				return e
			}
			if e = p.addSession(SessionRow{ID: auth.Hash(r.id), Kind: "cli_login", Status: "pending", UserID: v.UserID, Expires: migrationCreated(v.Expires)}, CLILoginData{IdentitySessionData: identity, LoopbackChallenge: NullableString(v.LoopbackChallenge), LoopbackRedirectURI: NullableString(v.LoopbackRedirectURI), LoopbackState: NullableString(v.LoopbackState), LoopbackCodeHash: NullableString(v.LoopbackCodeHash)}); e != nil {
				return e
			}
		case "oauth_codes":
			var v legacyV2OAuthCode
			if e := decodeLegacyJSON(r.body, &v); e != nil {
				return e
			}
			request, created, e := migrateOAuthRequest(v.Request)
			if e != nil {
				return e
			}
			var pkce json.RawMessage
			if len(v.PKCE) > 0 && string(v.PKCE) != "null" {
				pkce, _, e = migrateOAuthRequest(v.PKCE)
				if e != nil {
					return e
				}
			}
			status := "pending"
			if !v.Active {
				status = "active"
				pkce = nil
			}
			if v.ProjectIDs == nil {
				v.ProjectIDs = []string{}
			}
			if e = p.addSession(SessionRow{ID: r.id, Kind: "oauth_code", Status: status, UserID: v.UserID, Expires: migrationCreated(v.Expires), Created: created}, OAuthCodeData{ProjectIDs: v.ProjectIDs, Request: request, PKCE: pkce}); e != nil {
				return e
			}
		case "oauth_tokens":
			var v legacyV2OAuthGrant
			if e := decodeLegacyJSON(r.body, &v); e != nil {
				return e
			}
			if v.Kind != "access" && v.Kind != "refresh" {
				return errors.New("invalid legacy OAuth grant kind")
			}
			request, created, e := migrateOAuthRequest(v.Request)
			if e != nil {
				return e
			}
			if v.ProjectIDs == nil {
				v.ProjectIDs = []string{}
			}
			status := "active"
			if !v.Active {
				status = "revoked"
			}
			if e = p.addSession(SessionRow{ID: r.id, Kind: "mcp", Status: status, UserID: v.UserID, Expires: migrationCreated(v.Expires), Created: created}, MCPData{TokenKind: v.Kind, ClientID: v.ClientID, RequestID: v.RequestID, ProjectIDs: v.ProjectIDs, Request: request}); e != nil {
				return e
			}
		case "login_codes":
			var v legacyV2LoginCode
			if e := decodeLegacyJSON(r.body, &v); e != nil {
				return e
			}
			if e := p.addSession(SessionRow{ID: r.id, Kind: "login_code", Status: "pending", UserID: v.UserID, ProjectID: NullableString(v.ProjectID), Expires: migrationCreated(v.Expires)}, LoginCodeData{GrantKey: v.GrantKey, Role: NullableString(v.Role)}); e != nil {
				return e
			}
		case "oauth", "periods", "holds", "outbox", "alerts", "stripe_events":
			p.discardedRecords++
			continue
		default:
			return fmt.Errorf("unsupported legacy record kind: %s", r.kind)
		}
		p.migratedRecords++
	}
	return nil
}
func (p *migrationPlan) addSession(row SessionRow, data any) error {
	if row.ID == "" {
		return errors.New("missing legacy session key")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	row.Data = raw
	p.sessions = append(p.sessions, row)
	return nil
}
func migrateIdentity(cipher *auth.Cipher, user, provider, id, encrypted string) (IdentitySessionData, error) {
	data := IdentitySessionData{IdentityProvider: NullableString(provider), IdentityID: NullableString(id)}
	if encrypted == "" {
		return data, nil
	}
	if user == "" || provider == "" || id == "" {
		return data, errors.New("invalid legacy identity credential")
	}
	plain, err := cipher.Open(encrypted, "identity:"+user+":"+provider+":"+id)
	if err != nil {
		return data, errors.New("invalid legacy identity credential")
	}
	var value auth.Credential
	if decodeLegacyJSON([]byte(plain), &value) != nil || value.AccessToken == "" {
		return data, errors.New("invalid legacy identity credential")
	}
	tokens := cipher.SealIdentityTokens(value, user, provider, id)
	data.IdentityAccessToken = tokens.AccessToken
	data.IdentityRefreshToken = tokens.RefreshToken
	data.IdentityExpiresAt = tokens.ExpiresAt
	return data, nil
}
func migrateOAuthRequest(raw json.RawMessage) (json.RawMessage, int64, error) {
	// Validate the same types Fosite restores before committing the migration
	// and deleting its inputs. A missing ID would create a new replay family.
	value := fosite.Request{Client: &fosite.DefaultClient{}, Session: &fosite.DefaultSession{}}
	if err := json.Unmarshal(raw, &value); err != nil || value.ID == "" || value.Client == nil || value.Client.GetID() == "" || value.Session == nil {
		return nil, 0, errors.New("invalid legacy OAuth request")
	}
	return raw, migrationCreated(value.RequestedAt), nil
}
