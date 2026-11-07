package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

func googleConfig(purpose string) (oauth2.Endpoint, []string) {
	scopes := []string{oidc.ScopeOpenID, "email", "profile"}
	if purpose == "connect" {
		scopes = append(scopes, "https://www.googleapis.com/auth/documents.readonly", "https://www.googleapis.com/auth/drive.readonly")
	}
	return google.Endpoint, scopes
}

// Bind the OIDC nonce to the one-use pending verifier without storing another
// secret. Domain separation keeps it distinct from the public PKCE challenge.
func loginNonce(verifier string) string { return Challenge("kioku-oidc-nonce:" + verifier) }

func googleAuthOptions(verifier, purpose string) []oauth2.AuthCodeOption {
	opts := []oauth2.AuthCodeOption{oidc.Nonce(loginNonce(verifier))}
	if purpose == "connect" {
		opts = append(opts, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
	}
	return opts
}

func (p Provider) googleLoginIdentity(ctx context.Context, token *oauth2.Token, verifier string) (Identity, error) {
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, errors.New("missing Google ID token")
	}
	// Google publishes a fixed issuer and JWKS endpoint. go-oidc handles JWT
	// signature, issuer, audience and expiry validation; nonce remains ours.
	keys := oidc.NewRemoteKeySet(ctx, "https://www.googleapis.com/oauth2/v3/certs")
	verified, err := oidc.NewVerifier("https://accounts.google.com", keys, &oidc.Config{ClientID: p.ClientID}).Verify(ctx, raw)
	if err != nil {
		return Identity{}, err
	}
	if verified.Subject == "" || verified.Nonce != loginNonce(verifier) {
		return Identity{}, errors.New("invalid Google subject or nonce")
	}
	identity, err := p.googleIdentity(ctx, token.AccessToken)
	if err != nil {
		return Identity{}, err
	}
	if identity.ID != verified.Subject {
		return Identity{}, errors.New("Google UserInfo subject does not match ID token")
	}
	return identity, nil
}

func (p Provider) googleIdentity(ctx context.Context, token string) (Identity, error) {
	var r struct {
		Sub      string `json:"sub"`
		Name     string `json:"name"`
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err := p.get(ctx, "https://openidconnect.googleapis.com/v1/userinfo", token, &r); err != nil {
		return Identity{}, err
	}
	if r.Sub == "" || !r.Verified {
		return Identity{}, errors.New("a verified Google email is required")
	}
	return Identity{r.Sub, r.Name, strings.ToLower(r.Email)}, nil
}
