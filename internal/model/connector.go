package model

import (
	"encoding/json"
	"time"
)

type OAuthClient struct {
	ID           string    `json:"client_id"`
	Name         string    `json:"client_name"`
	RedirectURIs []string  `json:"redirect_uris"`
	Created      time.Time `json:"created"`
}

// OAuth records contain sanitized Fosite requests, never bearer tokens.
type OAuthCode struct {
	UserID     string          `json:"user_id"`
	ProjectIDs []string        `json:"project_ids"`
	Expires    time.Time       `json:"expires"`
	Active     bool            `json:"active"`
	Request    json.RawMessage `json:"request"`
	PKCE       json.RawMessage `json:"pkce,omitempty"`
}
type OAuthGrant struct {
	Kind       string          `json:"kind"`
	ClientID   string          `json:"client_id"`
	UserID     string          `json:"user_id"`
	ProjectIDs []string        `json:"project_ids"`
	Expires    time.Time       `json:"expires"`
	Active     bool            `json:"active"`
	RequestID  string          `json:"request_id"`
	Request    json.RawMessage `json:"request"`
}
type LoginCode struct {
	UserID    string    `json:"user_id"`
	ProjectID string    `json:"project_id"`
	GrantKey  string    `json:"grant_key"` // Hash of the issuing access token.
	Role      string    `json:"role"`
	Expires   time.Time `json:"expires"`
}
