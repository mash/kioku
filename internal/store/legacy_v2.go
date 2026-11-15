package store

import (
	"encoding/json"
	"time"

	"github.com/mash/kioku/internal/blob"
)

// These DTOs exist only for the stopped v2 migration. They are never read by
// normal v3 queries or used to write an old-format object.

type legacyV2Organization struct {
	ID      string            `json:"id"`
	Slug    string            `json:"slug"`
	Name    string            `json:"name"`
	Members map[string]string `json:"members"`
	Created time.Time         `json:"created"`
}

type legacyV2User struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Email      string            `json:"email"`
	Identities map[string]string `json:"identities,omitempty"`
}

type legacyV2Project struct {
	StorageBucket        string            `json:"storage_bucket,omitempty"`
	StorageRoot          string            `json:"storage_root,omitempty"`
	JobsValidAfter       time.Time         `json:"jobs_valid_after,omitempty"`
	WikiIncomplete       bool              `json:"wiki_incomplete,omitempty"`
	WikiCompleteRevision string            `json:"wiki_complete_revision,omitempty"`
	ViewEpoch            string            `json:"view_epoch,omitempty"`
	WikiUpdatedAt        time.Time         `json:"wiki_updated_at,omitempty"`
	NextGenerateAt       time.Time         `json:"next_generate_at,omitempty"`
	OrgID                string            `json:"org_id"`
	Slug                 string            `json:"slug"`
	Namespace            string            `json:"namespace"`
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	Members              map[string]string `json:"members"`
	Revision             string            `json:"revision"`
	Created              time.Time         `json:"created"`
	Deleting             bool              `json:"deleting,omitempty"`
}

type legacyV2Invite struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Email     string    `json:"email,omitempty"`
	CreatedBy string    `json:"created_by"`
	Expires   time.Time `json:"expires"`
	UsedBy    string    `json:"used_by,omitempty"`
	TokenHash string    `json:"token_hash"`
}

type legacyV2Session struct {
	JobID              string    `json:"job_id,omitempty"`
	AgentID            string    `json:"agent_id,omitempty"`
	ProjectID          string    `json:"project_id,omitempty"`
	Role               string    `json:"role,omitempty"`
	Created            time.Time `json:"created,omitempty"`
	IdentityProvider   string    `json:"identity_provider,omitempty"`
	IdentityID         string    `json:"identity_id,omitempty"`
	IdentityCredential string    `json:"identity_credential,omitempty"`
	UserID             string    `json:"user_id"`
	Expires            time.Time `json:"expires"`
}

type legacyV2OAuthState struct {
	ReturnPath string    `json:"return_path,omitempty"`
	Provider   string    `json:"provider"`
	Device     string    `json:"device,omitempty"`
	Verifier   string    `json:"verifier"`
	Expires    time.Time `json:"expires"`
}

type legacyV2Device struct {
	LoopbackChallenge   string    `json:"loopback_challenge,omitempty"`
	LoopbackRedirectURI string    `json:"loopback_redirect_uri,omitempty"`
	LoopbackState       string    `json:"loopback_state,omitempty"`
	LoopbackCodeHash    string    `json:"loopback_code_hash,omitempty"`
	IdentityProvider    string    `json:"identity_provider,omitempty"`
	IdentityID          string    `json:"identity_id,omitempty"`
	IdentityCredential  string    `json:"identity_credential,omitempty"`
	UserID              string    `json:"user_id,omitempty"`
	SecretHash          string    `json:"secret_hash"`
	Expires             time.Time `json:"expires"`
	Used                bool      `json:"used"`
}

type legacyV2Revision struct {
	// CompleteBytes retains the last completed tree's size during publication.
	// StoredBytes still accounts for the current, possibly partial tree.
	CompleteBytes *int64              `json:"complete_bytes,omitempty"`
	FileBytes     map[string]int64    `json:"file_bytes,omitempty"`
	FileHashes    map[string]string   `json:"file_hashes,omitempty"`
	StoredBytes   int64               `json:"stored_bytes"`
	ID            string              `json:"id"`
	ProjectID     string              `json:"project_id"`
	Created       time.Time           `json:"created"`
	Files         map[string]blob.Key `json:"files"` // relative filename -> current object key
}

type legacyV2Job struct {
	// Retained only to reject execution of retired BYOK jobs.
	AnthropicConnectionID string                  `json:"anthropic_connection_id,omitempty"`
	AgentBudgetCents      int64                   `json:"agent_budget_cents,omitempty"`
	Run                   *legacyV2AgentRun       `json:"run,omitempty"`
	Unchanged             bool                    `json:"unchanged,omitempty"`
	Kind                  string                  `json:"kind,omitempty"`
	Automatic             bool                    `json:"automatic,omitempty"`
	AgentID               string                  `json:"agent_id,omitempty"`
	ID                    string                  `json:"id"`
	ProjectID             string                  `json:"project_id"`
	UserID                string                  `json:"user_id"`
	Status                string                  `json:"status"`
	Error                 string                  `json:"error,omitempty"`
	Revision              string                  `json:"revision,omitempty"`
	Created               time.Time               `json:"created"`
	WikiStatus            string                  `json:"wiki_status,omitempty"`
	Views                 []legacyV2ViewJobResult `json:"views,omitempty"`
	ViewGeneration        *legacyV2ViewGeneration `json:"view_generation,omitempty"`
	ViewTask              *legacyV2ViewTask       `json:"view_task,omitempty"`
}

type legacyV2ViewJobResult struct {
	ViewID string `json:"view_id"`
	Slug   string `json:"slug"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type legacyV2ViewGeneration struct {
	Snapshot  string `json:"snapshot"`
	Pending   bool   `json:"pending,omitempty"`
	Remaining int64  `json:"remaining"`
	Next      int    `json:"next"`
	ChildID   string `json:"child_id,omitempty"`
}

type legacyV2ViewTask struct {
	ParentID string `json:"parent_id"`
	ViewID   string `json:"view_id"`
	Snapshot string `json:"snapshot"`
}

type legacyV2Audit struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	UserID    string    `json:"user_id"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	Created   time.Time `json:"created"`
}

type legacyV2InboxItem struct {
	Status    string    `json:"status"`
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	SourceURL string    `json:"source_url"`
	Remove    bool      `json:"remove,omitempty"`
	Submitter string    `json:"submitter"`
	Created   time.Time `json:"created"`
	RunID     string    `json:"run_id,omitempty"`
}

type legacyV2AgentToken struct {
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

type legacyV2AgentRun struct {
	InboxIDs        []string        `json:"inbox_ids"`
	Ref             legacyV2RunRef  `json:"ref"`
	CostCents       *int64          `json:"cost_cents,omitempty"`
	CleanupComplete bool            `json:"cleanup_complete,omitempty"`
	AbortReason     string          `json:"abort_reason,omitempty"`
	Phase           string          `json:"phase"`
	PreparedAt      time.Time       `json:"prepared_at"`
	Deadline        time.Time       `json:"deadline"`
	TokenID         string          `json:"token_id,omitempty"`
	Attempt         legacyV2Attempt `json:"attempt"`
	BudgetCents     int64           `json:"budget_cents"`
	BaseRevision    string          `json:"base_revision,omitempty"`
	OutputKey       blob.Key        `json:"output_key,omitempty"`
	OutputWaitSince *time.Time      `json:"output_wait_since,omitempty"`
	OutputHash      string          `json:"output_hash,omitempty"`
	OutputRejection string          `json:"output_rejection,omitempty"`
}

type legacyV2OAuthClient struct {
	ID           string    `json:"client_id"`
	Name         string    `json:"client_name"`
	RedirectURIs []string  `json:"redirect_uris"`
	Created      time.Time `json:"created"`
}

type legacyV2OAuthCode struct {
	UserID     string          `json:"user_id"`
	ProjectIDs []string        `json:"project_ids"`
	Expires    time.Time       `json:"expires"`
	Active     bool            `json:"active"`
	Request    json.RawMessage `json:"request"`
	PKCE       json.RawMessage `json:"pkce,omitempty"`
}

type legacyV2OAuthGrant struct {
	Kind       string          `json:"kind"`
	ClientID   string          `json:"client_id"`
	UserID     string          `json:"user_id"`
	ProjectIDs []string        `json:"project_ids"`
	Expires    time.Time       `json:"expires"`
	Active     bool            `json:"active"`
	RequestID  string          `json:"request_id"`
	Request    json.RawMessage `json:"request"`
}

type legacyV2LoginCode struct {
	UserID    string    `json:"user_id"`
	ProjectID string    `json:"project_id"`
	GrantKey  string    `json:"grant_key"` // Hash of the issuing access token.
	Role      string    `json:"role"`
	Expires   time.Time `json:"expires"`
}

type legacyV2Attempt struct {
	ID string `json:"id"`
}
type legacyV2RunRef struct {
	UsesProviderFiles bool              `json:"uses_provider_files"`
	ID                string            `json:"id"`
	Resources         map[string]string `json:"resources"`
}

type legacyV2Content struct {
	Inbox                map[string]legacyV2InboxItem `json:"inbox"`
	JobsValidAfter       time.Time                    `json:"jobs_valid_after,omitempty"`
	Schema               int                          `json:"schema"`
	ProjectID            string                       `json:"project_id"`
	WikiUpdatedAt        time.Time                    `json:"wiki_updated_at"`
	NextGenerateAt       time.Time                    `json:"next_generate_at"`
	Incomplete           bool                         `json:"incomplete"`
	WikiCompleteRevision string                       `json:"wiki_complete_revision,omitempty"`
	ViewEpoch            string                       `json:"view_epoch,omitempty"`
	Wiki                 *legacyV2Revision            `json:"wiki,omitempty"`
	Jobs                 map[string]legacyV2Job       `json:"jobs"`
	Audit                map[string]legacyV2Audit     `json:"audit"`
}
