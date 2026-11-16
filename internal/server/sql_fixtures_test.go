package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/store"
)

// These per-entity fixture values arrange or inspect typed SQL rows. They do
// not reconstruct a State snapshot or read any legacy stored representation.

type fixtureProject struct {
	WikiUpdatedAt  time.Time         `json:"wiki_updated_at,omitempty"`
	NextGenerateAt time.Time         `json:"next_generate_at,omitempty"`
	OrgID          string            `json:"org_id"`
	Slug           string            `json:"slug"`
	Namespace      string            `json:"namespace"`
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Members        map[string]string `json:"members"`
	Revision       string            `json:"revision"`
	Created        time.Time         `json:"created"`
	Deleting       bool              `json:"deleting,omitempty"`
}

type fixtureRevision struct {
	FileBytes   map[string]int64    `json:"file_bytes,omitempty"`
	FileHashes  map[string]string   `json:"file_hashes,omitempty"`
	StoredBytes int64               `json:"stored_bytes"`
	ID          string              `json:"id"`
	ProjectID   string              `json:"project_id"`
	Created     time.Time           `json:"created"`
	Files       map[string]blob.Key `json:"files"` // relative filename -> current object key
}

type fixtureJob struct {
	AgentBudgetCents int64                  `json:"agent_budget_cents,omitempty"`
	Run              *fixtureAgentRun       `json:"run,omitempty"`
	Unchanged        bool                   `json:"unchanged,omitempty"`
	Kind             string                 `json:"kind,omitempty"`
	Automatic        bool                   `json:"automatic,omitempty"`
	AgentID          string                 `json:"agent_id,omitempty"`
	ID               string                 `json:"id"`
	ProjectID        string                 `json:"project_id"`
	UserID           string                 `json:"user_id"`
	Status           string                 `json:"status"`
	Error            string                 `json:"error,omitempty"`
	Revision         string                 `json:"revision,omitempty"`
	Created          time.Time              `json:"created"`
	WikiStatus       string                 `json:"wiki_status,omitempty"`
	Views            []model.ViewJobResult  `json:"views,omitempty"`
	ViewGeneration   *fixtureViewGeneration `json:"view_generation,omitempty"`
	ViewTask         *fixtureViewTask       `json:"view_task,omitempty"`
}

type fixtureAgentRun struct {
	InboxIDs        []string           `json:"inbox_ids"`
	Ref             runner.RunRef      `json:"ref"`
	CostCents       *int64             `json:"cost_cents,omitempty"`
	CleanupComplete bool               `json:"cleanup_complete,omitempty"`
	AbortReason     string             `json:"abort_reason,omitempty"`
	Phase           model.ManagedPhase `json:"phase"`
	PreparedAt      time.Time          `json:"prepared_at"`
	Deadline        time.Time          `json:"deadline"`
	Attempt         fixtureAttempt     `json:"attempt"`
	BudgetCents     int64              `json:"budget_cents"`
	OutputKey       blob.Key           `json:"output_key,omitempty"`
	OutputWaitSince *time.Time         `json:"output_wait_since,omitempty"`
	OutputHash      string             `json:"output_hash,omitempty"`
	OutputRejection string             `json:"output_rejection,omitempty"`
}

type fixtureAttempt struct {
	ID string `json:"id"`
}

type fixtureViewGeneration struct {
	Snapshot  string `json:"snapshot"`
	Pending   bool   `json:"pending,omitempty"`
	Remaining int64  `json:"remaining"`
	Next      int    `json:"next"`
	ChildID   string `json:"child_id,omitempty"`
}

type fixtureViewTask struct {
	ParentID string `json:"parent_id"`
	ViewID   string `json:"view_id"`
	Snapshot string `json:"snapshot"`
}

type fixtureSession struct {
	JobID                string    `json:"job_id,omitempty"`
	AgentID              string    `json:"agent_id,omitempty"`
	ProjectID            string    `json:"project_id,omitempty"`
	Role                 string    `json:"role,omitempty"`
	Created              time.Time `json:"created,omitempty"`
	IdentityProvider     string    `json:"identity_provider,omitempty"`
	IdentityID           string    `json:"identity_id,omitempty"`
	IdentityAccessToken  string
	IdentityRefreshToken string
	UserID               string    `json:"user_id"`
	Expires              time.Time `json:"expires"`
}

type fixtureDevice struct {
	LoopbackChallenge    string `json:"loopback_challenge,omitempty"`
	LoopbackRedirectURI  string `json:"loopback_redirect_uri,omitempty"`
	LoopbackState        string `json:"loopback_state,omitempty"`
	LoopbackCodeHash     string `json:"loopback_code_hash,omitempty"`
	IdentityProvider     string `json:"identity_provider,omitempty"`
	IdentityID           string `json:"identity_id,omitempty"`
	IdentityAccessToken  string
	IdentityRefreshToken string
	UserID               string    `json:"user_id,omitempty"`
	SecretHash           string    `json:"secret_hash"`
	Expires              time.Time `json:"expires"`
}

type fixtureAgentToken struct {
	JobID     string    `json:"job_id,omitempty"`
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	TokenHash string    `json:"token_hash"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
}

type fixtureOAuthGrant struct {
	Kind       string          `json:"kind"`
	ClientID   string          `json:"client_id"`
	UserID     string          `json:"user_id"`
	ProjectIDs []string        `json:"project_ids"`
	Expires    time.Time       `json:"expires"`
	Active     bool            `json:"active"`
	RequestID  string          `json:"request_id"`
	Request    json.RawMessage `json:"request"`
}

type fixtureOAuthCode struct {
	UserID     string          `json:"user_id"`
	ProjectIDs []string        `json:"project_ids"`
	Expires    time.Time       `json:"expires"`
	Active     bool            `json:"active"`
	Request    json.RawMessage `json:"request"`
	PKCE       json.RawMessage `json:"pkce,omitempty"`
}

type fixtureLoginCode struct {
	UserID    string    `json:"user_id"`
	ProjectID string    `json:"project_id"`
	GrantKey  string    `json:"grant_key"` // Hash of the issuing access token.
	Role      string    `json:"role"`
	Expires   time.Time `json:"expires"`
}

type fixtureInvite struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Email     string    `json:"email,omitempty"`
	CreatedBy string    `json:"created_by"`
	Expires   time.Time `json:"expires"`
	UsedBy    string    `json:"used_by,omitempty"`
	TokenHash string    `json:"token_hash"`
}

func fixtureCheck(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func fixtureJSON(t testing.TB, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	fixtureCheck(t, err)
	return b
}
func openFixtureDatabase(dir string) (*store.Database, error) {
	path := filepath.Join(dir, "catalog.sqlite")
	_, err := os.Stat(path)
	db, err := store.OpenDatabase(path, os.IsNotExist(err))
	if err == nil {
		fixtureDatabasePaths.Store(db, path)
	}
	return db, err
}
func fixtureRow(j fixtureJob) store.JobRow {
	row := store.JobRow{ID: j.ID, ProjectID: j.ProjectID, Kind: j.Kind, Status: j.Status, Created: j.Created.UnixMilli(), Data: store.JobData{UserID: j.UserID, AgentID: store.NullableString(j.AgentID), AgentBudgetCents: j.AgentBudgetCents, Automatic: j.Automatic, Error: store.NullableString(j.Error), Revision: store.NullableString(j.Revision), WikiStatus: store.NullableString(j.WikiStatus), Unchanged: j.Unchanged, Views: []store.ViewResultData{}}}
	if j.Created.IsZero() {
		row.Created = 0
	}
	if row.Kind == "" {
		row.Kind = "generate"
	}
	for _, v := range j.Views {
		row.Data.Views = append(row.Data.Views, store.ViewResultData{ViewID: v.ViewID, Slug: v.Slug, Status: v.Status, Error: store.NullableString(v.Error)})
	}
	if j.ViewGeneration != nil {
		g := j.ViewGeneration
		row.GenerationSnapshot = &g.Snapshot
		row.Data.ViewGeneration = &store.GenerationData{Pending: g.Pending, Remaining: g.Remaining, Next: g.Next}
	}
	if j.ViewTask != nil {
		row.ParentID = &j.ViewTask.ParentID
		row.Data.ViewTask = &store.ViewTaskData{ViewID: j.ViewTask.ViewID}
	}
	if j.Run != nil {
		r := j.Run
		phase := string(r.Phase)
		if phase == "" {
			phase = "preparing"
		}
		row.Phase = &phase
		row.CleanupComplete = r.CleanupComplete
		row.InputReady = !r.CleanupComplete
		row.SessionID = store.NullableString(r.Ref.ID)
		row.OutputHash = store.NullableString(r.OutputHash)
		row.Data.Run = &store.RunData{UsesProviderFiles: r.Ref.UsesProviderFiles, UploadStarted: r.Ref.UploadStarted, InputFileID: store.NullableString(r.Ref.InputFileID), AbortReason: store.NullableString(r.AbortReason), PreparedAt: r.PreparedAt.UnixMilli(), Deadline: r.Deadline.UnixMilli(), OutputRejection: store.NullableString(r.OutputRejection), BudgetCents: r.BudgetCents}
		if r.OutputWaitSince != nil {
			n := r.OutputWaitSince.UnixMilli()
			row.Data.Run.OutputWaitSince = &n
		}
	}
	return row
}
func fixtureJobsFind(t testing.TB, source any, id string) (fixtureJob, bool) {
	if f, ok := source.(*fixture); ok {
		var value fixtureJob
		var found bool
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value, found = fixtureJobsFind(t, tx, id); return nil }))
		return value, found
	}
	q := source.(*store.Queries)
	t.Helper()
	row, err := q.Job(id)
	if errors.Is(err, sql.ErrNoRows) {
		return fixtureJob{}, false
	}
	fixtureCheck(t, err)
	return fixtureJobProjection(t, q, row), true
}
func fixtureJobsGet(t testing.TB, source any, id string) fixtureJob {
	if f, ok := source.(*fixture); ok {
		var value fixtureJob
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureJobsGet(t, tx, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	j, _ := fixtureJobsFind(t, q, id)
	return j
}
func fixtureJobProjection(t testing.TB, q *store.Queries, row store.JobRow) fixtureJob {
	pub, err := jobProjection(q, row)
	fixtureCheck(t, err)
	j := fixtureJob{ID: pub.ID, ProjectID: pub.ProjectID, Kind: pub.Kind, Status: pub.Status, Created: pub.Created, UserID: pub.UserID, AgentID: pub.AgentID, AgentBudgetCents: pub.AgentBudgetCents, Automatic: pub.Automatic, Error: pub.Error, Revision: pub.Revision, WikiStatus: pub.WikiStatus, Unchanged: pub.Unchanged, Views: pub.Views}
	if g := row.Data.ViewGeneration; g != nil {
		j.ViewGeneration = &fixtureViewGeneration{Snapshot: store.ValueString(row.GenerationSnapshot), Pending: g.Pending, Remaining: g.Remaining, Next: g.Next, ChildID: fixtureChildID(t, q, row.ID)}
	}
	if v := row.Data.ViewTask; v != nil {
		j.ViewTask = &fixtureViewTask{ParentID: store.ValueString(row.ParentID), ViewID: v.ViewID, Snapshot: fixtureParentSnapshot(t, q, row.ParentID)}
	}
	if r := row.Data.Run; r != nil {
		pinned, err := q.RunInbox(row.ProjectID, row.ID)
		fixtureCheck(t, err)
		ids := []string{}
		for _, item := range pinned {
			ids = append(ids, item.ID)
		}
		j.Run = &fixtureAgentRun{InboxIDs: ids, Ref: sqlAttempt(row).Run, Phase: model.ManagedPhase(store.ValueString(row.Phase)), CostCents: pub.Run.CostCents, CleanupComplete: row.CleanupComplete, AbortReason: store.ValueString(r.AbortReason), PreparedAt: storageDate(r.PreparedAt), Deadline: storageDate(r.Deadline), Attempt: fixtureAttempt{ID: row.ID}, BudgetCents: r.BudgetCents, OutputHash: store.ValueString(row.OutputHash), OutputRejection: store.ValueString(r.OutputRejection)}
		if row.OutputHash != nil {
			j.Run.OutputKey = sqlJobKey(row, "output-"+*row.OutputHash+".tar.gz")
		}
		if r.OutputWaitSince != nil {
			n := storageDate(*r.OutputWaitSince)
			j.Run.OutputWaitSince = &n
		}
	}
	return j
}
func fixtureJobsPut(t testing.TB, q *store.Queries, id string, j fixtureJob) {
	t.Helper()
	j.ID = id
	fixtureCheck(t, q.PutJob(fixtureRow(j)))
}

func fixtureJobsList(t testing.TB, source any, pids ...string) map[string]fixtureJob {
	if f, ok := source.(*fixture); ok {
		var value map[string]fixtureJob
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureJobsList(t, tx, pids...); return nil }))
		return value
	}
	q := source.(*store.Queries)
	out := map[string]fixtureJob{}
	for _, pid := range pids {
		rows, err := q.ProjectJobs(pid)
		fixtureCheck(t, err)
		for _, r := range rows {
			out[r.ID] = fixtureJobProjection(t, q, r)
		}
	}
	return out
}
func fixtureProjectsFind(t testing.TB, source any, id string) (fixtureProject, bool) {
	if f, ok := source.(*fixture); ok {
		var value fixtureProject
		var found bool
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value, found = fixtureProjectsFind(t, tx, id); return nil }))
		return value, found
	}
	q := source.(*store.Queries)
	row, err := q.Project(id)
	if errors.Is(err, sql.ErrNoRows) {
		return fixtureProject{}, false
	}
	fixtureCheck(t, err)
	p, err := projectProjection(q, row)
	fixtureCheck(t, err)
	return fixtureProject{ID: p.ID, OrgID: p.OrgID, Slug: p.Slug, Namespace: p.Namespace, Name: p.Name, Members: p.Members, Created: p.Created, Deleting: p.Deleting, Revision: p.Revision, WikiUpdatedAt: p.WikiUpdatedAt, NextGenerateAt: optionalDate(row.NextGenerateAt)}, true
}
func fixtureProjectsGet(t testing.TB, source any, id string) fixtureProject {
	if f, ok := source.(*fixture); ok {
		var value fixtureProject
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureProjectsGet(t, tx, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	p, _ := fixtureProjectsFind(t, q, id)
	return p
}

func fixtureProjectsPut(t testing.TB, q *store.Queries, id string, p fixtureProject) {
	row, err := q.Project(id)
	if !errors.Is(err, sql.ErrNoRows) {
		fixtureCheck(t, err)
	}
	row.ID = id
	row.OrgID = p.OrgID
	row.Slug = p.Slug
	row.Created = p.Created.UnixMilli()
	if p.Created.IsZero() {
		row.Created = 0
	}
	row.Deleting = p.Deleting
	row.WikiRevision = store.NullableString(p.Revision)
	row.NextGenerateAt = nil
	if !p.NextGenerateAt.IsZero() {
		n := p.NextGenerateAt.UnixMilli()
		row.NextGenerateAt = &n
	}
	row.Data.Name = p.Name
	row.Data.WikiUpdatedAt = nil
	if !p.WikiUpdatedAt.IsZero() {
		n := p.WikiUpdatedAt.UnixMilli()
		row.Data.WikiUpdatedAt = &n
	}
	fixtureCheck(t, q.PutProject(row))
	members, err := q.ProjectMembers(id)
	fixtureCheck(t, err)
	for user := range members {
		fixtureCheck(t, q.DeleteProjectMember(id, user))
	}
	for user, role := range p.Members {
		fixtureCheck(t, q.PutProjectMember(id, user, role))
	}
}
func fixtureUsersGet(t testing.TB, source any, id string) model.User {
	if f, ok := source.(*fixture); ok {
		var value model.User
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureUsersGet(t, tx, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	u, _ := fixtureUsersFind(t, q, id)
	return u
}
func fixtureUsersFind(t testing.TB, source any, id string) (model.User, bool) {
	if f, ok := source.(*fixture); ok {
		var value model.User
		var found bool
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value, found = fixtureUsersFind(t, tx, id); return nil }))
		return value, found
	}
	q := source.(*store.Queries)
	row, err := q.User(id)
	if errors.Is(err, sql.ErrNoRows) {
		return model.User{}, false
	}
	fixtureCheck(t, err)
	u, err := userProjection(q, row)
	fixtureCheck(t, err)
	return u, true
}
func fixtureUsersPut(t testing.TB, q *store.Queries, id string, u model.User) {
	fixtureCheck(t, q.PutUser(store.UserRow{ID: id, Email: u.Email, Data: store.UserData{Name: u.Name}}))
	ids, err := q.Identities(id)
	fixtureCheck(t, err)
	for p := range ids {
		fixtureCheck(t, q.DeleteIdentity(id, p))
	}
	for p, v := range u.Identities {
		fixtureCheck(t, q.PutIdentity(id, p, v))
	}
}

func fixtureOrganizationsGet(t testing.TB, source any, id string) model.Organization {
	if f, ok := source.(*fixture); ok {
		var value model.Organization
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureOrganizationsGet(t, tx, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	row, err := q.Organization(id)
	fixtureCheck(t, err)
	o, err := organizationProjection(q, row)
	fixtureCheck(t, err)
	return o
}
func fixtureInboxGet(t testing.TB, source any, id string) model.InboxItem {
	if f, ok := source.(*fixture); ok {
		var value model.InboxItem
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureInboxGet(t, tx, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	v, _ := fixtureInboxFind(t, q, id)
	return v
}
func fixtureInboxFind(t testing.TB, source any, id string) (model.InboxItem, bool) {
	if f, ok := source.(*fixture); ok {
		var value model.InboxItem
		var found bool
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value, found = fixtureInboxFind(t, tx, id); return nil }))
		return value, found
	}
	q := source.(*store.Queries)
	row, err := q.Inbox(id)
	if errors.Is(err, sql.ErrNoRows) {
		return model.InboxItem{}, false
	}
	fixtureCheck(t, err)
	return inboxProjection(row), true
}
func fixtureInboxList(t testing.TB, source any, pids ...string) map[string]model.InboxItem {
	if f, ok := source.(*fixture); ok {
		var value map[string]model.InboxItem
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureInboxList(t, tx, pids...); return nil }))
		return value
	}
	q := source.(*store.Queries)
	out := map[string]model.InboxItem{}
	for _, id := range pids {
		rows, err := q.ProjectInbox(id)
		fixtureCheck(t, err)
		for _, r := range rows {
			out[r.ID] = inboxProjection(r)
		}
	}
	return out
}
func fixtureInboxPut(t testing.TB, q *store.Queries, id string, v model.InboxItem) {
	created := int64(0)
	if !v.Created.IsZero() {
		created = v.Created.UnixMilli()
	}
	fixtureCheck(t, q.PutInbox(store.InboxRow{ID: id, ProjectID: v.ProjectID, SourceURL: v.SourceURL, Remove: v.Remove, RunID: store.NullableString(v.RunID), Created: created, Data: store.InboxData{Submitter: v.Submitter}}))
}
func fixtureRevisionsGet(t testing.TB, source any, pid, id string) fixtureRevision {
	if f, ok := source.(*fixture); ok {
		var value fixtureRevision
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureRevisionsGet(t, tx, pid, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	for _, r := range fixtureRevisionsList(t, q, pid) {
		if r.ID == id {
			return r
		}
	}
	return fixtureRevision{}
}
func fixtureRevisionsList(t testing.TB, source any, pids ...string) map[string]fixtureRevision {
	if f, ok := source.(*fixture); ok {
		var value map[string]fixtureRevision
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureRevisionsList(t, tx, pids...); return nil }))
		return value
	}
	q := source.(*store.Queries)
	out := map[string]fixtureRevision{}
	for _, pid := range pids {
		p, err := q.Project(pid)
		fixtureCheck(t, err)
		if p.WikiRevision == nil {
			continue
		}
		r, err := wikiProjection(q, p)
		fixtureCheck(t, err)
		v := fixtureRevision{ID: r.Revision, ProjectID: pid, Files: map[string]blob.Key{}, FileHashes: map[string]string{}, FileBytes: map[string]int64{}}
		for name, row := range r.Files {
			v.Files[name], v.FileHashes[name], v.FileBytes[name] = wikiKey(row), row.Hash, row.Data.Bytes
			v.StoredBytes += row.Data.Bytes
		}
		out[r.Revision] = v
	}
	return out
}
func fixtureAuditList(t testing.TB, source any, pids ...string) map[string]model.Audit {
	if f, ok := source.(*fixture); ok {
		var value map[string]model.Audit
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureAuditList(t, tx, pids...); return nil }))
		return value
	}
	q := source.(*store.Queries)
	out := map[string]model.Audit{}
	for _, pid := range pids {
		rows, err := q.ProjectAudit(pid)
		fixtureCheck(t, err)
		for _, r := range rows {
			out[r.ID] = model.Audit{ID: r.ID, ProjectID: pid, UserID: r.Data.UserID, Action: r.Data.Action, Detail: r.Data.Detail, Created: storageDate(r.Created)}
		}
	}
	return out
}
func fixtureSessionsGet(t testing.TB, source any, id string) fixtureSession {
	if f, ok := source.(*fixture); ok {
		var value fixtureSession
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureSessionsGet(t, tx, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	v, _ := fixtureSessionsFind(t, q, id)
	return v
}
func fixtureSessionsFind(t testing.TB, source any, id string) (fixtureSession, bool) {
	if f, ok := source.(*fixture); ok {
		var value fixtureSession
		var found bool
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value, found = fixtureSessionsFind(t, tx, id); return nil }))
		return value, found
	}
	q := source.(*store.Queries)
	row, err := q.Session(id)
	if errors.Is(err, sql.ErrNoRows) {
		return fixtureSession{}, false
	}
	fixtureCheck(t, err)
	var d store.IdentitySessionData
	fixtureCheck(t, json.Unmarshal(row.Data, &d))
	return fixtureSession{UserID: row.UserID, ProjectID: store.ValueString(row.ProjectID), Created: creationDate(row.Created), Expires: storageDate(row.Expires), IdentityProvider: store.ValueString(d.IdentityProvider), IdentityID: store.ValueString(d.IdentityID), IdentityAccessToken: store.ValueString(d.IdentityAccessToken), IdentityRefreshToken: store.ValueString(d.IdentityRefreshToken)}, true
}
func fixtureSessionsPut(t testing.TB, q *store.Queries, id string, v fixtureSession) {
	old, err := q.Session(id)
	if !errors.Is(err, sql.ErrNoRows) {
		fixtureCheck(t, err)
	}
	var data store.IdentitySessionData
	if err == nil {
		fixtureCheck(t, json.Unmarshal(old.Data, &data))
	}
	data.IdentityProvider = store.NullableString(v.IdentityProvider)
	data.IdentityID = store.NullableString(v.IdentityID)
	data.IdentityAccessToken = store.NullableString(v.IdentityAccessToken)
	data.IdentityRefreshToken = store.NullableString(v.IdentityRefreshToken)
	kind := old.Kind
	if kind == "" {
		kind = "user"
	}
	created := int64(0)
	if !v.Created.IsZero() {
		created = v.Created.UnixMilli()
	}
	fixtureCheck(t, q.PutSession(store.SessionRow{ID: id, Kind: kind, Status: "active", UserID: v.UserID, Expires: v.Expires.UnixMilli(), Created: created, Data: fixtureJSON(t, data)}))
}
func fixtureSessionsList(t testing.TB, source any, uid string) map[string]fixtureSession {
	if f, ok := source.(*fixture); ok {
		var value map[string]fixtureSession
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureSessionsList(t, tx, uid); return nil }))
		return value
	}
	q := source.(*store.Queries)
	out := map[string]fixtureSession{}
	rows, err := q.UserSessions(uid, "user")
	fixtureCheck(t, err)
	for _, r := range rows {
		out[r.ID] = fixtureSessionsGet(t, q, r.ID)
	}
	return out
}
func fixtureDevicesGet(t testing.TB, source any, id string) fixtureDevice {
	if f, ok := source.(*fixture); ok {
		var value fixtureDevice
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureDevicesGet(t, tx, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	row, err := q.Session(auth.Hash(id))
	if errors.Is(err, sql.ErrNoRows) {
		return fixtureDevice{}
	}
	fixtureCheck(t, err)
	var d store.CLILoginData
	fixtureCheck(t, json.Unmarshal(row.Data, &d))
	return fixtureDevice{UserID: row.UserID, SecretHash: row.ID, Expires: storageDate(row.Expires), IdentityProvider: store.ValueString(d.IdentityProvider), IdentityID: store.ValueString(d.IdentityID), IdentityAccessToken: store.ValueString(d.IdentityAccessToken), LoopbackChallenge: store.ValueString(d.LoopbackChallenge), LoopbackRedirectURI: store.ValueString(d.LoopbackRedirectURI), LoopbackState: store.ValueString(d.LoopbackState), LoopbackCodeHash: store.ValueString(d.LoopbackCodeHash)}
}
func fixtureDevicesPut(t testing.TB, q *store.Queries, id string, v fixtureDevice) {
	key := auth.Hash(id)
	old, err := q.Session(key)
	if !errors.Is(err, sql.ErrNoRows) {
		fixtureCheck(t, err)
	}
	var d store.CLILoginData
	if err == nil {
		fixtureCheck(t, json.Unmarshal(old.Data, &d))
	}
	d.IdentityProvider = store.NullableString(v.IdentityProvider)
	d.IdentityID = store.NullableString(v.IdentityID)
	d.LoopbackChallenge = store.NullableString(v.LoopbackChallenge)
	d.LoopbackRedirectURI = store.NullableString(v.LoopbackRedirectURI)
	d.LoopbackState = store.NullableString(v.LoopbackState)
	d.LoopbackCodeHash = store.NullableString(v.LoopbackCodeHash)
	fixtureCheck(t, q.PutSession(store.SessionRow{ID: key, Kind: "cli_login", Status: "pending", UserID: v.UserID, Expires: v.Expires.UnixMilli(), Data: fixtureJSON(t, d)}))
}
func fixtureDevicesDelete(t testing.TB, q *store.Queries, id string) {
	fixtureCheck(t, q.DeleteSession(auth.Hash(id)))
}
func fixtureAgentTokensList(t testing.TB, source any, pid string) map[string]fixtureAgentToken {
	if f, ok := source.(*fixture); ok {
		var value map[string]fixtureAgentToken
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureAgentTokensList(t, tx, pid); return nil }))
		return value
	}
	q := source.(*store.Queries)
	rows, err := q.ProjectSessions(pid, "agent")
	fixtureCheck(t, err)
	out := map[string]fixtureAgentToken{}
	for _, row := range rows {
		var d store.AgentSessionData
		fixtureCheck(t, json.Unmarshal(row.Data, &d))
		id := store.ValueString(d.AgentID)
		out[id] = fixtureAgentToken{ID: id, ProjectID: store.ValueString(row.ProjectID), UserID: row.UserID, Name: d.Name, Role: store.ValueString(d.Role), TokenHash: row.ID, Created: creationDate(row.Created), Expires: storageDate(row.Expires)}
	}
	return out
}
func fixtureAgentTokensGet(t testing.TB, source any, pid, id string) fixtureAgentToken {
	if f, ok := source.(*fixture); ok {
		var value fixtureAgentToken
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureAgentTokensGet(t, tx, pid, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	return fixtureAgentTokensList(t, q, pid)[id]
}
func fixtureAgentTokensPut(t testing.TB, q *store.Queries, id string, v fixtureAgentToken) {
	v.ID = id
	d := store.AgentSessionData{SessionRoleData: store.SessionRoleData{AgentID: &id, Role: store.NullableString(v.Role)}, Name: v.Name}
	created := int64(0)
	if !v.Created.IsZero() {
		created = v.Created.UnixMilli()
	}
	fixtureCheck(t, q.PutSession(store.SessionRow{ID: v.TokenHash, Kind: "agent", Status: "active", UserID: v.UserID, ProjectID: &v.ProjectID, Created: created, Expires: v.Expires.UnixMilli(), Data: fixtureJSON(t, d)}))
}
func fixtureOAuthTokensGet(t testing.TB, source any, id string) fixtureOAuthGrant {
	if f, ok := source.(*fixture); ok {
		var value fixtureOAuthGrant
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureOAuthTokensGet(t, tx, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	row, err := q.Session(id)
	fixtureCheck(t, err)
	var d store.MCPData
	fixtureCheck(t, json.Unmarshal(row.Data, &d))
	return fixtureOAuthGrant{Kind: d.TokenKind, ClientID: d.ClientID, UserID: row.UserID, ProjectIDs: d.ProjectIDs, Expires: storageDate(row.Expires), Active: row.Status == "active", RequestID: d.RequestID, Request: d.Request}
}
func fixtureOAuthTokensPut(t testing.TB, q *store.Queries, id string, v fixtureOAuthGrant) {
	status := "active"
	if !v.Active {
		status = "revoked"
	}
	fixtureCheck(t, q.PutSession(store.SessionRow{ID: id, Kind: "mcp", Status: status, UserID: v.UserID, Expires: v.Expires.UnixMilli(), Data: fixtureJSON(t, store.MCPData{TokenKind: v.Kind, ClientID: v.ClientID, ProjectIDs: v.ProjectIDs, RequestID: v.RequestID, Request: v.Request})}))
}
func fixtureOAuthTokensList(t testing.TB, source any, uid string) map[string]fixtureOAuthGrant {
	if f, ok := source.(*fixture); ok {
		var value map[string]fixtureOAuthGrant
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureOAuthTokensList(t, tx, uid); return nil }))
		return value
	}
	q := source.(*store.Queries)
	rows, err := q.UserSessions(uid, "mcp")
	fixtureCheck(t, err)
	out := map[string]fixtureOAuthGrant{}
	for _, row := range rows {
		out[row.ID] = fixtureOAuthTokensGet(t, q, row.ID)
	}
	return out
}
func fixtureOAuthTokensDelete(t testing.TB, q *store.Queries, id string) {
	fixtureCheck(t, q.DeleteSession(id))
}
func fixtureOAuthCodesGet(t testing.TB, source any, id string) fixtureOAuthCode {
	if f, ok := source.(*fixture); ok {
		var value fixtureOAuthCode
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureOAuthCodesGet(t, tx, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	row, err := q.Session(id)
	fixtureCheck(t, err)
	var d store.OAuthCodeData
	fixtureCheck(t, json.Unmarshal(row.Data, &d))
	return fixtureOAuthCode{UserID: row.UserID, ProjectIDs: d.ProjectIDs, Expires: storageDate(row.Expires), Active: row.Status == "pending", Request: d.Request, PKCE: d.PKCE}
}
func fixtureOAuthCodesPut(t testing.TB, q *store.Queries, id string, v fixtureOAuthCode) {
	status := "active"
	if v.Active {
		status = "pending"
	}
	fixtureCheck(t, q.PutSession(store.SessionRow{ID: id, Kind: "oauth_code", Status: status, UserID: v.UserID, Expires: v.Expires.UnixMilli(), Data: fixtureJSON(t, store.OAuthCodeData{ProjectIDs: v.ProjectIDs, Request: v.Request, PKCE: v.PKCE})}))
}
func fixtureOAuthCodesList(t testing.TB, source any, uid string) map[string]fixtureOAuthCode {
	if f, ok := source.(*fixture); ok {
		var value map[string]fixtureOAuthCode
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureOAuthCodesList(t, tx, uid); return nil }))
		return value
	}
	q := source.(*store.Queries)
	rows, err := q.UserSessions(uid, "oauth_code")
	fixtureCheck(t, err)
	out := map[string]fixtureOAuthCode{}
	for _, row := range rows {
		out[row.ID] = fixtureOAuthCodesGet(t, q, row.ID)
	}
	return out
}
func fixtureLoginCodesGet(t testing.TB, source any, id string) fixtureLoginCode {
	if f, ok := source.(*fixture); ok {
		var value fixtureLoginCode
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureLoginCodesGet(t, tx, id); return nil }))
		return value
	}
	q := source.(*store.Queries)
	row, err := q.Session(id)
	fixtureCheck(t, err)
	var d store.LoginCodeData
	fixtureCheck(t, json.Unmarshal(row.Data, &d))
	return fixtureLoginCode{UserID: row.UserID, ProjectID: store.ValueString(row.ProjectID), GrantKey: d.GrantKey, Role: store.ValueString(d.Role), Expires: storageDate(row.Expires)}
}
func fixtureLoginCodesPut(t testing.TB, q *store.Queries, id string, v fixtureLoginCode) {
	fixtureCheck(t, q.PutSession(store.SessionRow{ID: id, Kind: "login_code", Status: "pending", UserID: v.UserID, ProjectID: &v.ProjectID, Expires: v.Expires.UnixMilli(), Data: fixtureJSON(t, store.LoginCodeData{GrantKey: v.GrantKey, Role: &v.Role})}))
}
func fixtureLoginCodesList(t testing.TB, source any, uid string) map[string]fixtureLoginCode {
	if f, ok := source.(*fixture); ok {
		var value map[string]fixtureLoginCode
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureLoginCodesList(t, tx, uid); return nil }))
		return value
	}
	q := source.(*store.Queries)
	rows, err := q.UserSessions(uid, "login_code")
	fixtureCheck(t, err)
	out := map[string]fixtureLoginCode{}
	for _, row := range rows {
		out[row.ID] = fixtureLoginCodesGet(t, q, row.ID)
	}
	return out
}
func fixtureInvitesList(t testing.TB, source any, pids ...string) map[string]fixtureInvite {
	if f, ok := source.(*fixture); ok {
		var value map[string]fixtureInvite
		fixtureCheck(t, f.stateStore.Read(context.Background(), func(tx *store.Queries) error { value = fixtureInvitesList(t, tx, pids...); return nil }))
		return value
	}
	q := source.(*store.Queries)
	out := map[string]fixtureInvite{}
	for _, pid := range pids {
		rows, err := q.ProjectInvites(pid)
		fixtureCheck(t, err)
		for _, row := range rows {
			out[row.ID] = fixtureInvite{ID: row.ID, ProjectID: pid, Email: store.ValueString(row.Data.Email), CreatedBy: row.CreatedBy, Expires: storageDate(row.Expires), UsedBy: store.ValueString(row.UsedBy), TokenHash: row.TokenHash}
		}
	}
	return out
}
func fixtureJobKey(j fixtureJob, name string) blob.Key { return sqlJobKey(fixtureRow(j), name) }
func fixtureJobPrefix(j fixtureJob) string {
	return "projects/" + j.ProjectID + "/.kioku/jobs/" + j.ID + "/"
}

type fixtureProjectEntry struct {
	fixtureProject
	InboxCount int                `json:"inbox_count"`
	Inbox      []model.InboxEntry `json:"inbox"`
}

// fixtureRevisionsPut arranges only SQL references; bodies must be PUT first.
func fixtureRevisionsPut(t testing.TB, q *store.Queries, id string, v fixtureRevision) {
	p, err := q.Project(v.ProjectID)
	fixtureCheck(t, err)
	p.WikiRevision = &id
	fixtureCheck(t, q.PutProject(p))
	old, err := q.ProjectWiki(v.ProjectID)
	fixtureCheck(t, err)
	for _, row := range old {
		fixtureCheck(t, q.DeleteWikiFile(v.ProjectID, row.Path))
	}
	for name, key := range v.Files {
		if !strings.HasPrefix(name, "wiki/") {
			continue
		}
		hash := v.FileHashes[name]
		if hash == "" {
			hash = strings.TrimSuffix(path.Base(string(key)), ".md")
		}
		fixtureCheck(t, q.PutWikiFile(store.WikiFileRow{ProjectID: v.ProjectID, Path: name, Hash: hash, Data: store.WikiFileData{Bytes: v.FileBytes[name]}}))
	}
}
func sessionKindCount(t *testing.T, f *fixture, kind string) int {
	t.Helper()
	db := fixtureSQLDatabase(t, f)
	var n int
	fixtureCheck(t, db.QueryRow("SELECT count(*) FROM sessions WHERE kind=?", kind).Scan(&n))
	return n
}

func fixtureViewRow(t testing.TB, f *fixture, pid, id string) store.ViewRow {
	t.Helper()
	var v store.ViewRow
	fixtureCheck(t, f.stateStore.Read(context.Background(), func(q *store.Queries) error { var err error; v, err = q.View(pid, id); return err }))
	return v
}
func fixtureViewCreated(t testing.TB, f *fixture, pid, id string, created time.Time) {
	t.Helper()
	fixtureCheck(t, f.stateStore.Write(context.Background(), func(q *store.Queries) error {
		v, err := q.View(pid, id)
		if err != nil {
			return err
		}
		n := created.UnixMilli()
		v.CreatedAt = &n
		return q.PutView(v)
	}))
}

// fixtureViewBundle arranges an existing publication without exercising a job.
func fixtureViewBundle(t testing.TB, f *fixture, pid, id, jid, snapshot string, body []byte) {
	t.Helper()
	hash := model.HashContent(body)
	fixtureCheck(t, f.s.Blobs.Put(context.Background(), model.ViewContentKey(pid, id, hash), body))
	fixtureCheck(t, f.stateStore.Write(context.Background(), func(q *store.Queries) error {
		v, err := q.View(pid, id)
		if err != nil {
			return err
		}
		_, err = q.Project(pid)
		if err != nil {
			return err
		}
		v.BundleHash = store.NullableString(string(hash))
		n := time.Now().UnixMilli()
		v.Data.Status = store.NullableString("completed")
		v.Data.JobID = &jid
		v.Data.WikiSnapshotID = &snapshot
		v.Data.GeneratedAt = &n
		return q.PutView(v)
	}))
}

func viewsKey(pid string, suffix string) blob.Key {
	return blob.Key("projects/" + pid + "/.kioku/views/" + suffix)
}

func viewKey(pid, id, name string) blob.Key {
	return viewsKey(pid, id+"/"+name)
}

func fixtureChildID(t testing.TB, q *store.Queries, parent string) string {
	t.Helper()
	child, err := q.GenerationChild(parent)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	fixtureCheck(t, err)
	return child.ID
}
func fixtureParentSnapshot(t testing.TB, q *store.Queries, parent *string) string {
	t.Helper()
	if parent == nil {
		return ""
	}
	row, err := q.Job(*parent)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	fixtureCheck(t, err)
	return store.ValueString(row.GenerationSnapshot)
}
func readSQLChildID(t *testing.T, o *storageOperations, parent string) string {
	t.Helper()
	var id string
	fixtureCheck(t, o.db.Read(t.Context(), func(q *store.Queries) error { id = fixtureChildID(t, q, parent); return nil }))
	return id
}

func fixtureOversizedWiki(t testing.TB, f *fixture, q *store.Queries, pid, revision string) {
	t.Helper()
	f.s.Config.MaxWikiBytes = 32 << 10
	body := []byte(strings.Repeat("x", int(f.s.Config.MaxWikiBytes)+1))
	hash := model.HashContent(body)
	key := model.WikiContentKey(pid, hash)
	fixtureCheck(t, f.s.Blobs.Put(context.Background(), key, body))
	fixtureRevisionsPut(t, q, revision, fixtureRevision{ID: revision, ProjectID: pid, Files: map[string]blob.Key{"wiki/index.md": key}, FileHashes: map[string]string{"wiki/index.md": string(hash)}, FileBytes: map[string]int64{"wiki/index.md": int64(len(body))}})
}
