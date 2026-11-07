// Package connector defines the contract implemented by external connectors.
package connector

import (
	"context"

	"golang.org/x/oauth2"
)

type Identity struct{ ID, Name, Email string }

// Get sends an authenticated GET request and decodes the JSON response into out.
type Get func(ctx context.Context, url, accessToken string, out any) error

// OAuth contains the connector-specific settings for authorization and token requests.
type OAuth struct {
	Endpoint oauth2.Endpoint
	Scopes   []string
	Options  []oauth2.AuthCodeOption
}

// Connector owns service-specific OAuth settings and identity verification.
type Connector interface {
	OAuth(purpose, verifier string) OAuth
	LoginIdentity(ctx context.Context, get Get, clientID string, token *oauth2.Token, verifier string) (Identity, error)
	Identity(ctx context.Context, get Get, accessToken string) (Identity, error)
}
