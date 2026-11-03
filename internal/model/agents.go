package model

import "time"

// AgentToken is a revocable delegation, never a user login or provider credential.
// TokenHash is persisted only; API responses use an explicit public projection.
type AgentToken struct {
	JobID     string    `json:"job_id,omitempty"`
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	TokenHash string    `json:"token_hash"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
	RevokedAt time.Time `json:"revoked_at,omitempty"`
}
