package google

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/mash/kioku/internal/connector"
	"golang.org/x/oauth2"
	oauthprovider "golang.org/x/oauth2/google"
)

func (Connector) OAuth(purpose, verifier string) connector.OAuth {
	scopes := []string{oidc.ScopeOpenID, "email", "profile"}
	if purpose == "connect" {
		scopes = append(scopes, "https://www.googleapis.com/auth/documents.readonly", "https://www.googleapis.com/auth/drive.readonly")
	}
	endpoint := oauthprovider.Endpoint
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return connector.OAuth{Endpoint: endpoint, Scopes: scopes, Options: authOptions(verifier, purpose)}
}

// Bind the OIDC nonce to the one-use pending verifier without storing another
// secret. Domain separation keeps it distinct from the public PKCE challenge.
func loginNonce(verifier string) string {
	h := sha256.Sum256([]byte("kioku-oidc-nonce:" + verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func authOptions(verifier, purpose string) []oauth2.AuthCodeOption {
	opts := []oauth2.AuthCodeOption{oidc.Nonce(loginNonce(verifier))}
	if purpose == "connect" {
		opts = append(opts, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
	}
	return opts
}

func (p Connector) LoginIdentity(ctx context.Context, get connector.Get, clientID string, token *oauth2.Token, verifier string) (connector.Identity, error) {
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return connector.Identity{}, errors.New("missing Google ID token")
	}
	// Google publishes a fixed issuer and JWKS endpoint. go-oidc handles JWT
	// signature, issuer, audience and expiry validation; nonce remains ours.
	keys := oidc.NewRemoteKeySet(ctx, "https://www.googleapis.com/oauth2/v3/certs")
	verified, err := oidc.NewVerifier("https://accounts.google.com", keys, &oidc.Config{ClientID: clientID}).Verify(ctx, raw)
	if err != nil {
		return connector.Identity{}, err
	}
	if verified.Subject == "" || verified.Nonce != loginNonce(verifier) {
		return connector.Identity{}, errors.New("invalid Google subject or nonce")
	}
	identity, err := p.Identity(ctx, get, token.AccessToken)
	if err != nil {
		return connector.Identity{}, err
	}
	if identity.ID != verified.Subject {
		return connector.Identity{}, errors.New("Google UserInfo subject does not match ID token")
	}
	return identity, nil
}

func (p Connector) Identity(ctx context.Context, get connector.Get, token string) (connector.Identity, error) {
	var r struct {
		Sub      string `json:"sub"`
		Name     string `json:"name"`
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err := get(ctx, "https://openidconnect.googleapis.com/v1/userinfo", token, &r); err != nil {
		return connector.Identity{}, err
	}
	if r.Sub == "" || !r.Verified {
		return connector.Identity{}, errors.New("a verified Google email is required")
	}
	return connector.Identity{ID: r.Sub, Name: r.Name, Email: strings.ToLower(r.Email)}, nil
}

type Connector struct{}

var _ connector.Connector = Connector{}
