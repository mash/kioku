package model

import (
	"time"

	"github.com/mash/kioku/internal/blob"
)

type Organization struct {
	ID      string            `json:"id"`
	Slug    string            `json:"slug"`
	Name    string            `json:"name"`
	Members map[string]string `json:"members"`
	Created time.Time         `json:"created"`
}

type User struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Email      string            `json:"email"`
	Identities map[string]string `json:"identities,omitempty"`
}
type Project struct {
	WikiIncomplete       bool              `json:"wiki_incomplete,omitempty"`
	WikiCompleteRevision string            `json:"wiki_complete_revision,omitempty"`
	ViewEpoch            string            `json:"view_epoch,omitempty"`
	WikiUpdatedAt        time.Time         `json:"wiki_updated_at,omitempty"`
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

// ProjectEntry adds current inbox state to a project listing without persisting it.
type ProjectEntry struct {
	Project
	InboxCount int          `json:"inbox_count"`
	Inbox      []InboxEntry `json:"inbox"`
}

type InboxEntry struct {
	SourceURL string `json:"source_url"`
	Remove    bool   `json:"remove,omitempty"`
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
	AgentBudgetCents int64           `json:"agent_budget_cents,omitempty"`
	Run              *AgentRun       `json:"run,omitempty"`
	Unchanged        bool            `json:"unchanged,omitempty"`
	Kind             string          `json:"kind,omitempty"`
	Automatic        bool            `json:"automatic,omitempty"`
	AgentID          string          `json:"agent_id,omitempty"`
	ID               string          `json:"id"`
	ProjectID        string          `json:"project_id"`
	UserID           string          `json:"user_id"`
	Status           string          `json:"status"`
	Error            string          `json:"error,omitempty"`
	Revision         string          `json:"revision,omitempty"`
	Created          time.Time       `json:"created"`
	WikiStatus       string          `json:"wiki_status,omitempty"`
	Views            []ViewJobResult `json:"views,omitempty"`
}

type ViewJobResult struct {
	ViewID string `json:"view_id"`
	Slug   string `json:"slug"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type Audit struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	UserID    string    `json:"user_id"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	Created   time.Time `json:"created"`
}

type FileInput struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}
type PutRequest struct {
	File      *FileInput `json:"file,omitempty"`
	Remove    bool       `json:"remove,omitempty"`
	SourceURL string     `json:"source_url,omitempty"`
}

type InboxItem struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	SourceURL string    `json:"source_url"`
	Remove    bool      `json:"remove,omitempty"`
	Submitter string    `json:"submitter"`
	Created   time.Time `json:"created"`
	RunID     string    `json:"run_id,omitempty"`
}

func (i InboxItem) BodyKey() blob.Key {
	return blob.Key("projects/" + i.ProjectID + "/.kioku/inbox/" + i.ID + ".md")
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
