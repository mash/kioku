package model

import (
	"time"

	"github.com/mash/kioku/internal/runner"
)

// GitHub credentials are encrypted control records, never project content.
type GitHubAppConnection struct {
	AppID                             int64
	GitHubUserID                      int64
	Login                             string
	Version                           uint64
	Status                            string
	EncryptedCredential               string
	AccessExpiresAt, RefreshExpiresAt time.Time
	CredentialGeneration              uint64
	RefreshState                      string
	RefreshStartedAt                  time.Time
	Installations                     map[int64]GitHubAppInstallation
}

type GitHubAppInstallation struct {
	ID        int64     `json:"id"`
	AccountID int64     `json:"account_id"`
	Login     string    `json:"login"`
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checked_at"`
	Reason    string    `json:"reason,omitempty"`
}

type GitHubAppConnectAttempt struct {
	UserID, SessionHash                         string
	Version                                     uint64
	StateHash, BrowserHash, CSRFHash, SetupHash string
	EncryptedCredential                         string
	GitHubUserID                                int64
	Login                                       string
	AccessExpiresAt, RefreshExpiresAt           time.Time
	Installations                               map[int64]GitHubAppInstallation
	Status                                      string
}

type GitHubAttemptAuthorization struct {
	Key                            runner.AttemptKey
	OwnerID                        string
	ConnectionVersion              uint64
	InvalidatedAt                  time.Time
	Selections                     []GitHubRepositorySelection
	SelectionCounted, FetchCounted bool
}

type GitHubRepositorySelection struct {
	Repository  string `json:"repo"`
	Mode        string `json:"mode"`
	Reason      string `json:"reason"`
	Placeholder string `json:"placeholder,omitempty"`
}

type GitHubTokenAttempt struct {
	Key                                                                runner.AttemptKey
	OwnerID                                                            string
	ConnectionVersion                                                  uint64
	InstallationID                                                     int64
	RepositoryIDs                                                      []int64
	Permissions                                                        map[string]string
	ScopeDigest, Status, EncryptedToken                                string
	ExpiresAt, JWTExpiresAt, AuthorizationCheckedAt, IssuanceStartedAt time.Time
	RevocationRequestedAt, NextAttemptAt                               time.Time
	Failures                                                           int
}

type GitHubMetrics struct {
	Counters          map[string]uint64
	RateRemaining     map[string]int64
	MigrationComplete bool
}
