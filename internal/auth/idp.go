package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"golang.org/x/oauth2/google"
)

type Identity struct{ ID, Name, Email string }

// idp holds the settings and identity verification of one identity provider.
type idp struct {
	endpoint oauth2.Endpoint
	scopes   []string
	options  func(verifier string) []oauth2.AuthCodeOption
	// login verifies the identity of a fresh login token.
	login    func(ctx context.Context, p Provider, token *oauth2.Token, verifier string) (Identity, error)
	identity func(ctx context.Context, p Provider, accessToken string) (Identity, error)
}

var idps = map[string]idp{
	"github": {
		endpoint: inParams(github.Endpoint),
		scopes:   []string{"read:user", "user:email"},
		options:  func(string) []oauth2.AuthCodeOption { return nil },
		login: func(ctx context.Context, p Provider, token *oauth2.Token, _ string) (Identity, error) {
			return githubIdentity(ctx, p, token.AccessToken)
		},
		identity: githubIdentity,
	},
	"google": {
		endpoint: inParams(google.Endpoint),
		scopes:   []string{oidc.ScopeOpenID, "email", "profile"},
		options: func(verifier string) []oauth2.AuthCodeOption {
			return []oauth2.AuthCodeOption{oidc.Nonce(loginNonce(verifier))}
		},
		login:    googleLoginIdentity,
		identity: googleIdentity,
	},
}

// ProviderNames returns the supported identity provider names.
func ProviderNames() []string { return slices.Sorted(maps.Keys(idps)) }

func (p Provider) idp() (idp, error) {
	v, ok := idps[p.Name]
	if !ok {
		return idp{}, fmt.Errorf("unsupported provider %q", p.Name)
	}
	return v, nil
}

func inParams(e oauth2.Endpoint) oauth2.Endpoint {
	e.AuthStyle = oauth2.AuthStyleInParams
	return e
}

func githubIdentity(ctx context.Context, p Provider, token string) (Identity, error) {
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := p.get(ctx, "https://api.github.com/user", token, &u); err != nil {
		return Identity{}, err
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := p.get(ctx, "https://api.github.com/user/emails", token, &emails); err != nil {
		return Identity{}, err
	}
	if u.ID == 0 {
		return Identity{}, errors.New("invalid GitHub user")
	}
	if u.Name == "" {
		u.Name = u.Login
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			return Identity{ID: fmt.Sprint(u.ID), Name: u.Name, Email: strings.ToLower(e.Email)}, nil
		}
	}
	return Identity{}, errors.New("a verified primary GitHub email is required")
}

// Bind the OIDC nonce to the one-use pending verifier without storing another
// secret. Domain separation keeps it distinct from the public PKCE challenge.
func loginNonce(verifier string) string {
	h := sha256.Sum256([]byte("kioku-oidc-nonce:" + verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func googleLoginIdentity(ctx context.Context, p Provider, token *oauth2.Token, verifier string) (Identity, error) {
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
	identity, err := googleIdentity(ctx, p, token.AccessToken)
	if err != nil {
		return Identity{}, err
	}
	if identity.ID != verified.Subject {
		return Identity{}, errors.New("Google UserInfo subject does not match ID token")
	}
	return identity, nil
}

func googleIdentity(ctx context.Context, p Provider, token string) (Identity, error) {
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
	return Identity{ID: r.Sub, Name: r.Name, Email: strings.ToLower(r.Email)}, nil
}
