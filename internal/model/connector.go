package model

import "time"

type OAuthClient struct {
	ID           string    `json:"client_id"`
	Name         string    `json:"client_name"`
	RedirectURIs []string  `json:"redirect_uris"`
	Created      time.Time `json:"created"`
}
type OAuthCode struct {
	ClientID    string    `json:"client_id"`
	UserID      string    `json:"user_id"`
	ProjectIDs  []string  `json:"project_ids"`
	RedirectURI string    `json:"redirect_uri"`
	Challenge   string    `json:"challenge"`
	Expires     time.Time `json:"expires"`
}
type OAuthGrant struct {
	Kind       string    `json:"kind"`
	ClientID   string    `json:"client_id"`
	UserID     string    `json:"user_id"`
	ProjectIDs []string  `json:"project_ids"`
	Expires    time.Time `json:"expires"`
}
type LoginCode struct {
	UserID    string    `json:"user_id"`
	ProjectID string    `json:"project_id"`
	GrantKey  string    `json:"grant_key"` // Hash of the issuing access token.
	Role      string    `json:"role"`
	Expires   time.Time `json:"expires"`
}
