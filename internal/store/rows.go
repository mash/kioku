package store

import (
	"encoding/json"

	"github.com/mash/kioku/internal/model"
)

// Row types separate indexed columns from non-searchable payloads.
type rowScanner interface{ Scan(...any) error }
type OrganizationRow struct {
	ID   string
	Slug string
	Data OrganizationData
}

func scanOrganization(s rowScanner) (OrganizationRow, error) {
	var v OrganizationRow
	var raw string
	err := s.Scan(&v.ID, &v.Slug, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

type UserRow struct {
	ID    string
	Email string
	Data  UserData
}

func scanUser(s rowScanner) (UserRow, error) {
	var v UserRow
	var raw string
	err := s.Scan(&v.ID, &v.Email, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

type ProjectRow struct {
	ID             string
	OrgID          string
	Slug           string
	Created        int64
	Deleting       bool
	WikiRevision   *string
	WikiIncomplete bool
	NextGenerateAt *int64
	ViewEpoch      *string
	Data           ProjectData
}

func scanProject(s rowScanner) (ProjectRow, error) {
	var v ProjectRow
	var raw string
	err := s.Scan(&v.ID, &v.OrgID, &v.Slug, &v.Created, &v.Deleting, &v.WikiRevision, &v.WikiIncomplete, &v.NextGenerateAt, &v.ViewEpoch, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

type SessionRow struct {
	ID        string
	Kind      string
	Status    string
	UserID    string
	ProjectID *string
	Expires   int64
	Created   int64
	Data      json.RawMessage
}

func scanSession(s rowScanner) (SessionRow, error) {
	var v SessionRow
	var raw string
	err := s.Scan(&v.ID, &v.Kind, &v.Status, &v.UserID, &v.ProjectID, &v.Expires, &v.Created, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

type InviteRow struct {
	ID        string
	ProjectID string
	CreatedBy string
	Expires   int64
	UsedBy    *string
	TokenHash string
	Data      InviteData
}

func scanInvite(s rowScanner) (InviteRow, error) {
	var v InviteRow
	var raw string
	err := s.Scan(&v.ID, &v.ProjectID, &v.CreatedBy, &v.Expires, &v.UsedBy, &v.TokenHash, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

type OAuthClientRow struct {
	ClientID string
	Data     OAuthClientData
}

func scanOAuthClient(s rowScanner) (OAuthClientRow, error) {
	var v OAuthClientRow
	var raw string
	err := s.Scan(&v.ClientID, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

type InboxRow struct {
	ID        string
	ProjectID string
	SourceURL string
	Remove    bool
	Created   int64
	RunID     *string
	Data      InboxData
}

func scanInbox(s rowScanner) (InboxRow, error) {
	var v InboxRow
	var raw string
	err := s.Scan(&v.ID, &v.ProjectID, &v.SourceURL, &v.Remove, &v.Created, &v.RunID, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

type WikiFileRow struct {
	ProjectID string
	Path      string
	Hash      string
	Data      WikiFileData
}

func scanWikiFile(s rowScanner) (WikiFileRow, error) {
	var v WikiFileRow
	var raw string
	err := s.Scan(&v.ProjectID, &v.Path, &v.Hash, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

type JobRow struct {
	ID                 string
	ProjectID          string
	Status             string
	Created            int64
	Kind               string
	SessionID          *string
	InputReady         bool
	CleanupComplete    *bool
	Phase              *string
	OutputHash         *string
	GenerationSnapshot *string
	ChildID            *string
	ParentID           *string
	Data               JobData
}

func scanJob(s rowScanner) (JobRow, error) {
	var v JobRow
	var raw string
	err := s.Scan(&v.ID, &v.ProjectID, &v.Status, &v.Created, &v.Kind, &v.SessionID, &v.InputReady, &v.CleanupComplete, &v.Phase, &v.OutputHash, &v.GenerationSnapshot, &v.ChildID, &v.ParentID, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

type AuditRow struct {
	ID        string
	ProjectID string
	Created   int64
	Data      AuditData
}

func scanAudit(s rowScanner) (AuditRow, error) {
	var v AuditRow
	var raw string
	err := s.Scan(&v.ID, &v.ProjectID, &v.Created, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

type ViewRow struct {
	ProjectID  string
	ID         string
	Slug       string
	CreatedAt  *int64
	BundleHash *string
	ViewEpoch  *string
	Data       ViewData
}

func scanView(s rowScanner) (ViewRow, error) {
	var v ViewRow
	var raw string
	err := s.Scan(&v.ProjectID, &v.ID, &v.Slug, &v.CreatedAt, &v.BundleHash, &v.ViewEpoch, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

type UsageRow struct {
	AttemptID  string
	ProjectID  string
	JobID      string
	RecordedAt int64
	Data       model.UsageValues
}

func scanUsage(s rowScanner) (UsageRow, error) {
	var v UsageRow
	var raw string
	err := s.Scan(&v.AttemptID, &v.ProjectID, &v.JobID, &v.RecordedAt, &raw)
	if err != nil {
		return v, err
	}
	err = unmarshalData(raw, &v.Data)
	return v, err
}

func listRows[T any](q *Queries, query string, scan func(rowScanner) (T, error), args ...any) ([]T, error) {
	rows, err := q.rows(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (v SessionRow) Active(now int64) bool { return v.Status == "active" && v.Expires > now }
func (v JobRow) Running() bool             { return v.Status == "running" && v.Data.Run != nil }
