package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/mash/kioku/internal/blob"
)

type Organization struct {
	ID      string            `json:"id"`
	Slug    string            `json:"slug"`
	Name    string            `json:"name"`
	Members map[string]string `json:"members"`
	Created time.Time         `json:"created"`
	// retained holds JSON fields that no current field reads, such as the
	// Stripe fields of earlier versions. A save writes them back unchanged.
	retained map[string]json.RawMessage
}

type organizationFields Organization

func (o *Organization) UnmarshalJSON(b []byte) error {
	var fields organizationFields
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	t := reflect.TypeOf(fields)
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		delete(all, name)
	}
	fields.retained = nil
	if len(all) > 0 {
		fields.retained = all
	}
	*o = Organization(fields)
	return nil
}

func (o Organization) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(organizationFields(o))
	if err != nil || len(o.retained) == 0 {
		return b, err
	}
	var all map[string]json.RawMessage
	if err = json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	for k, v := range o.retained {
		if _, known := all[k]; !known {
			all[k] = v
		}
	}
	return json.Marshal(all)
}

type User struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Email       string            `json:"email"`
	Identities  map[string]string `json:"identities,omitempty"`
	Connections map[string]string `json:"connections,omitempty"` // encrypted OAuth credentials
}
type Project struct {
	StorageBucket           string            `json:"storage_bucket,omitempty"`
	StorageRoot             string            `json:"storage_root,omitempty"`
	JobsValidAfter          time.Time         `json:"jobs_valid_after,omitempty"`
	WikiIncomplete          bool              `json:"wiki_incomplete,omitempty"`
	WikiUpdatedAt           time.Time         `json:"wiki_updated_at,omitempty"`
	NextGenerateAt          time.Time         `json:"next_generate_at,omitempty"`
	GenerateIntervalSeconds int64             `json:"generate_interval_seconds,omitempty"`
	OrgID                   string            `json:"org_id"`
	Slug                    string            `json:"slug"`
	Namespace               string            `json:"namespace"`
	ID                      string            `json:"id"`
	Name                    string            `json:"name"`
	Members                 map[string]string `json:"members"`
	Revision                string            `json:"revision"`
	Created                 time.Time         `json:"created"`
	Deleting                bool              `json:"deleting,omitempty"`
}

// ProjectEntry adds current inbox state to a project listing without persisting it.
type ProjectEntry struct {
	Project
	InboxCount int          `json:"inbox_count"`
	Inbox      []InboxEntry `json:"inbox"`
}

type InboxEntry struct {
	SourceURL string `json:"source_url"`
	Status    string `json:"status"`
}

type Invite struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Email     string    `json:"email,omitempty"`
	CreatedBy string    `json:"created_by"`
	Expires   time.Time `json:"expires"`
	UsedBy    string    `json:"used_by,omitempty"`
	TokenHash string    `json:"token_hash"`
}
type Session struct {
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
type OAuthState struct {
	ReturnPath string    `json:"return_path,omitempty"`
	Provider   string    `json:"provider"`
	Purpose    string    `json:"purpose"`
	UserID     string    `json:"user_id,omitempty"`
	Device     string    `json:"device,omitempty"`
	Verifier   string    `json:"verifier"`
	Expires    time.Time `json:"expires"`
}
type Device struct {
	Purpose             string    `json:"purpose,omitempty"`
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
type Revision struct {
	FileBytes   map[string]int64    `json:"file_bytes,omitempty"`
	FileHashes  map[string]string   `json:"file_hashes,omitempty"`
	StoredBytes int64               `json:"stored_bytes"`
	ID          string              `json:"id"`
	ProjectID   string              `json:"project_id"`
	Created     time.Time           `json:"created"`
	Files       map[string]blob.Key `json:"files"` // relative filename -> current object key
}
type Job struct {
	// Retained only to reject execution/retry of retired BYOK jobs.
	AnthropicConnectionID string    `json:"anthropic_connection_id,omitempty"`
	AgentBudgetCents      int64     `json:"agent_budget_cents,omitempty"`
	Run                   *AgentRun `json:"run,omitempty"`
	Unchanged             bool      `json:"unchanged,omitempty"`
	Kind                  string    `json:"kind,omitempty"`
	Automatic             bool      `json:"automatic,omitempty"`
	AgentID               string    `json:"agent_id,omitempty"`
	ID                    string    `json:"id"`
	ProjectID             string    `json:"project_id"`
	UserID                string    `json:"user_id"`
	Status                string    `json:"status"`
	Error                 string    `json:"error,omitempty"`
	Revision              string    `json:"revision,omitempty"`
	Created               time.Time `json:"created"`
}
type State struct {
	Inbox         map[string]InboxItem    `json:"inbox"`
	OAuthClients  map[string]OAuthClient  `json:"oauth_clients"`
	OAuthCodes    map[string]OAuthCode    `json:"oauth_codes"`
	OAuthTokens   map[string]OAuthGrant   `json:"oauth_tokens"`
	LoginCodes    map[string]LoginCode    `json:"login_codes"`
	AgentTokens   map[string]AgentToken   `json:"agent_tokens"`
	Organizations map[string]Organization `json:"organizations"`
	Audit         map[string]Audit        `json:"audit"`
	Users         map[string]User         `json:"users"`
	Projects      map[string]Project      `json:"projects"`
	Invites       map[string]Invite       `json:"invites"`
	Sessions      map[string]Session      `json:"sessions"`
	OAuth         map[string]OAuthState   `json:"oauth"`
	Devices       map[string]Device       `json:"devices"`
	Revisions     map[string]Revision     `json:"revisions"`
	Jobs          map[string]Job          `json:"jobs"`
}
type Audit struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	UserID    string    `json:"user_id"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	Created   time.Time `json:"created"`
}

func NewState() State {
	return State{Inbox: map[string]InboxItem{}, OAuthClients: map[string]OAuthClient{}, OAuthCodes: map[string]OAuthCode{}, OAuthTokens: map[string]OAuthGrant{}, LoginCodes: map[string]LoginCode{}, AgentTokens: map[string]AgentToken{}, Organizations: map[string]Organization{}, Audit: map[string]Audit{}, Users: map[string]User{}, Projects: map[string]Project{}, Invites: map[string]Invite{}, Sessions: map[string]Session{}, OAuth: map[string]OAuthState{}, Devices: map[string]Device{}, Revisions: map[string]Revision{}, Jobs: map[string]Job{}}
}

type FileInput struct {
	SourceURL string `json:"source_url,omitempty"`
	Name      string `json:"name"`
	Content   string `json:"content"`
}
type PutRequest struct {
	URL       string     `json:"url,omitempty"`
	File      *FileInput `json:"file,omitempty"`
	Remove    bool       `json:"remove,omitempty"`
	SourceURL string     `json:"source_url,omitempty"`
}

type InboxItem struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	SourceURL string    `json:"source_url"`
	Status    string    `json:"status"`
	Submitter string    `json:"submitter"`
	Created   time.Time `json:"created"`
	RunID     string    `json:"run_id,omitempty"`
}

func (i InboxItem) BodyKey() blob.Key {
	return blob.Key("projects/" + i.ProjectID + "/.kioku/inbox/" + i.ID + ".md")
}

type Snapshot struct {
	Revision string            `json:"revision"`
	Files    map[string]string `json:"files"`
}

// SyncManifest describes the current published tree, including partial publication.
type SyncManifest struct {
	Revision string              `json:"revision"`
	Files    map[string]SyncFile `json:"files"`
}
type SyncFile struct {
	Hash  string `json:"hash"`
	Bytes int64  `json:"bytes"`
}
type SnapshotFile struct {
	Body string `json:"body"`
	Hash string `json:"hash"`
}
