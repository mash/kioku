package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

// Objects avoids an import cycle with the concrete local and S3 implementations.
type Objects = blob.Store
type fileObjects struct{ dir string }

func (f fileObjects) Put(_ context.Context, key blob.Key, b []byte) error {
	path := filepath.Join(f.dir, filepath.FromSlash(string(key)))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return AtomicWrite(path, b, 0600)
}
func (f fileObjects) Get(_ context.Context, key blob.Key) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(string(key))))
}
func (f fileObjects) Delete(_ context.Context, key blob.Key) error {
	err := os.Remove(filepath.Join(f.dir, filepath.FromSlash(string(key))))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (f fileObjects) List(_ context.Context, prefix string) ([]blob.Key, error) {
	var out []blob.Key
	err := filepath.WalkDir(f.dir, func(path string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		key, _ := filepath.Rel(f.dir, path)
		key = filepath.ToSlash(key)
		if strings.HasPrefix(key, prefix) {
			out = append(out, blob.Key(key))
		}
		return nil
	})
	return out, err
}
func missingObject(err error) bool {
	var e interface{ NotFound() bool }
	return os.IsNotExist(err) || errors.As(err, &e) && e.NotFound()
}

// Only these fields are persisted for a project in SQLite.
type projectConfig struct {
	ID            string            `json:"id"`
	OrgID         string            `json:"org_id"`
	Slug          string            `json:"slug"`
	Namespace     string            `json:"namespace"`
	Name          string            `json:"name"`
	Members       map[string]string `json:"members"`
	Created       time.Time         `json:"created"`
	Deleting      bool              `json:"deleting,omitempty"`
	StorageBucket string            `json:"storage_bucket,omitempty"`
	StorageRoot   string            `json:"storage_root"`
}

func projectConfigs(st model.State) map[string]projectConfig {
	out := map[string]projectConfig{}
	for id, p := range st.Projects {
		root := p.StorageRoot
		if root == "" {
			root = "projects/" + id + "/"
		}
		out[id] = projectConfig{p.ID, p.OrgID, p.Slug, p.Namespace, p.Name, p.Members, p.Created, p.Deleting, p.StorageBucket, root}
	}
	return out
}
func controlJSON(st model.State) ([]byte, error) {
	return json.Marshal(map[string]any{
		"github_app_connections": st.GitHubAppConnections, "github_app_connect_attempts": st.GitHubAppConnectAttempts, "github_attempt_authorizations": st.GitHubAttemptAuthorizations, "github_token_attempts": st.GitHubTokenAttempts, "github_metrics": st.GitHubMetrics, "oauth_clients": st.OAuthClients, "oauth_codes": st.OAuthCodes, "oauth_tokens": st.OAuthTokens, "login_codes": st.LoginCodes,
		"users": st.Users, "projects": projectConfigs(st), "organizations": st.Organizations,
		"agent_tokens": st.AgentTokens, "invites": st.Invites, "sessions": st.Sessions, "oauth": st.OAuth, "devices": st.Devices,
	})
}

// projectContent contains no account configuration, membership or credentials.
type projectContent struct {
	Inbox                   map[string]model.InboxItem `json:"inbox"`
	JobsValidAfter          time.Time                  `json:"jobs_valid_after,omitempty"`
	Schema                  int                        `json:"schema"`
	ProjectID               string                     `json:"project_id"`
	WikiUpdatedAt           time.Time                  `json:"wiki_updated_at"`
	NextGenerateAt          time.Time                  `json:"next_generate_at"`
	GenerateIntervalSeconds int64                      `json:"generate_interval_seconds"`
	Incomplete              bool                       `json:"incomplete"`
	Wiki                    *model.Revision            `json:"wiki,omitempty"`
	Jobs                    map[string]model.Job       `json:"jobs"`
	Audit                   map[string]model.Audit     `json:"audit"`
}

func contentFor(st model.State, pid string) projectContent {
	p := st.Projects[pid]
	c := projectContent{Inbox: map[string]model.InboxItem{}, JobsValidAfter: p.JobsValidAfter, Schema: 2, ProjectID: pid, WikiUpdatedAt: p.WikiUpdatedAt, NextGenerateAt: p.NextGenerateAt, GenerateIntervalSeconds: p.GenerateIntervalSeconds, Incomplete: p.WikiIncomplete, Jobs: map[string]model.Job{}, Audit: map[string]model.Audit{}}
	if current, ok := st.Revisions[p.Revision]; ok {
		c.Wiki = &current
	}
	for id, v := range st.Jobs {
		if v.ProjectID == pid {
			c.Jobs[id] = v
		}
	}
	for id, v := range st.Audit {
		if v.ProjectID == pid {
			c.Audit[id] = v
		}
	}
	for id, v := range st.Inbox {
		if v.ProjectID == pid {
			c.Inbox[id] = v
		}
	}
	return c
}
func contentKey(pid string) blob.Key { return blob.Key("projects/" + pid + "/.kioku/state.json") }
func (s *Store) loadContent(pid string) error {
	if !validID(pid) {
		return errors.New("invalid project identity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := s.objects.Get(ctx, contentKey(pid))
	if err != nil {
		return err
	}
	var c projectContent
	if err = json.Unmarshal(b, &c); err != nil {
		return err
	}
	if c.ProjectID != pid {
		return errors.New("project content identity mismatch")
	}
	if c.Schema == 1 {
		return errors.New("old project content (schema 1) must be deleted by the operator and replaced with empty schema 2 content before startup")
	}
	if c.Schema != 2 {
		return errors.New("unsupported project content schema")
	}
	for id, v := range c.Inbox {
		if v.ProjectID != pid || id != v.ID || !validID(id) {
			return errors.New("cross-project inbox in content")
		}
		if prior, exists := s.state.Inbox[id]; exists && prior.ProjectID != pid {
			return errors.New("duplicate inbox identity across projects")
		}
	}
	if c.Wiki != nil && c.Wiki.ProjectID != pid {
		return errors.New("cross-project wiki in content")
	}
	for id, v := range c.Jobs {
		if v.ProjectID != pid || id != v.ID || !validID(id) {
			return errors.New("cross-project job in content")
		}
	}
	for _, v := range c.Audit {
		if v.ProjectID != pid {
			return errors.New("cross-project audit in content")
		}
	}
	if c.Wiki != nil {
		for _, key := range c.Wiki.Files {
			if !strings.HasPrefix(string(key), "projects/"+pid+"/") {
				return errors.New("cross-project wiki object reference")
			}
		}
	}
	for id, v := range c.Inbox {
		s.state.Inbox[id] = v
	}
	p := s.state.Projects[pid]
	p.JobsValidAfter = c.JobsValidAfter
	p.WikiUpdatedAt = c.WikiUpdatedAt
	p.NextGenerateAt = c.NextGenerateAt
	p.GenerateIntervalSeconds = c.GenerateIntervalSeconds
	p.Revision = ""
	if c.Wiki != nil {
		p.Revision = c.Wiki.ID
	}
	p.WikiIncomplete = c.Incomplete
	s.state.Projects[pid] = p
	if c.Wiki != nil {
		s.state.Revisions[c.Wiki.ID] = *c.Wiki
	}
	for id, v := range c.Jobs {
		s.state.Jobs[id] = v
	}
	for id, v := range c.Audit {
		s.state.Audit[id] = v
	}
	return nil
}
func (s *Store) saveContent(st model.State, pid string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := contentFor(st, pid)
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if len(b) > 16<<20 {
		return errors.New("project working index exceeds the 16 MiB object limit")
	}
	if err = s.objects.Put(ctx, contentKey(pid), b); err != nil {
		return err
	}
	return nil
}

type controlIntent struct {
	Control     json.RawMessage `json:"control"`
	Transaction string          `json:"transaction"`
	Projects    []string        `json:"projects"`
}

func transactionKey(pid, id string) blob.Key {
	return blob.Key("projects/" + pid + "/.kioku/transactions/" + id + ".json")
}
func (s *Store) persist(st model.State) error {
	if err := s.archiveChangedRecords(st); err != nil {
		return err
	}
	compactContent(&st)
	before, _ := controlJSON(s.state)
	after, err := controlJSON(st)
	if err != nil {
		return err
	}
	var changed []string
	for pid := range st.Projects {
		a, _ := json.Marshal(contentFor(s.state, pid))
		b, _ := json.Marshal(contentFor(st, pid))
		if _, ok := s.state.Projects[pid]; !ok || !bytes.Equal(a, b) {
			changed = append(changed, pid)
		}
	}
	sort.Strings(changed)
	if bytes.Equal(before, after) {
		for _, pid := range changed {
			if err = s.saveContent(st, pid); err != nil {
				return err
			}
		}
		return nil
	}
	// Control-only redo intent carries references to external content, never its
	// source URLs, wiki metadata, jobs or processing checkpoints.
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return err
	}
	intent := controlIntent{Control: after, Transaction: hex.EncodeToString(token[:]), Projects: changed}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, pid := range changed {
		b, _ := json.Marshal(contentFor(st, pid))
		if len(b) > 16<<20 {
			return errors.New("project working index exceeds the 16 MiB object limit")
		}
		if err = s.objects.Put(ctx, transactionKey(pid, intent.Transaction), b); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(intent)
	if _, err = s.catalog.Exec("INSERT OR REPLACE INTO pending(id,payload)VALUES(1,?)", raw); err != nil {
		return err
	}
	if err = s.recoverIntent(raw); err != nil {
		return err
	}
	return nil
}
func (s *Store) recoverIntent(raw []byte) error {
	var in controlIntent
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	if len(in.Control) == 0 || !validID(in.Transaction) {
		return errors.New("invalid control transaction intent")
	}
	config := model.NewState()
	if err := json.Unmarshal(in.Control, &config); err != nil {
		return err
	}
	registerBuckets(s.objects, config)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, pid := range in.Projects {
		if !validID(pid) {
			return errors.New("invalid project in intent")
		}
		b, err := s.objects.Get(ctx, transactionKey(pid, in.Transaction))
		if err != nil {
			return err
		}
		var c projectContent
		if err = json.Unmarshal(b, &c); err != nil {
			return err
		}
		if c.Schema != 2 || c.ProjectID != pid {
			return errors.New("invalid transaction content")
		}
		if err = s.objects.Put(ctx, contentKey(pid), b); err != nil {
			return err
		}
	}
	st := model.NewState()
	if err := json.Unmarshal(in.Control, &st); err != nil {
		return err
	}
	if err := writeRecords(s.catalog, st); err != nil {
		return err
	}
	for _, pid := range in.Projects {
		_ = s.objects.Delete(ctx, transactionKey(pid, in.Transaction))
	}
	return nil
}

func registerBuckets(objects Objects, st model.State) {
	if router, ok := objects.(interface{ RegisterProjectBucket(string, string) }); ok {
		for id, p := range st.Projects {
			router.RegisterProjectBucket(id, p.StorageBucket)
		}
	}
}

const hotRecords = 256

// The job and audit archives live in object storage. Keep only recent records
// in the working index so a busy project's state.json does not grow forever.
func compactContent(st *model.State) {
	for pid := range st.Projects {
		var jobs []model.Job
		for _, j := range st.Jobs {
			if j.ProjectID == pid && j.Status != "running" && j.Status != "queued" && (j.Run == nil || j.Run.CleanupComplete) {
				jobs = append(jobs, j)
			}
		}
		sort.Slice(jobs, func(i, j int) bool {
			if jobs[i].Created.Equal(jobs[j].Created) {
				return jobs[i].ID > jobs[j].ID
			}
			return jobs[i].Created.After(jobs[j].Created)
		})
		if len(jobs) > hotRecords {
			for _, j := range jobs[hotRecords:] {
				delete(st.Jobs, j.ID)
			}
		}
		var audit []model.Audit
		for _, e := range st.Audit {
			if e.ProjectID == pid {
				audit = append(audit, e)
			}
		}
		sort.Slice(audit, func(i, j int) bool { return audit[i].Created.After(audit[j].Created) })
		if len(audit) > hotRecords {
			for _, e := range audit[hotRecords:] {
				delete(st.Audit, e.ID)
			}
		}
	}
}
func (s *Store) archiveChangedRecords(st model.State) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return s.archiveRecords(ctx, st)
}
func (s *Store) archiveRecords(ctx context.Context, st model.State) error {
	put := func(key blob.Key, old, record any) error {
		before, _ := json.Marshal(old)
		after, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if bytes.Equal(before, after) {
			return nil
		}
		return s.objects.Put(ctx, key, after)
	}
	for id, j := range st.Jobs {
		if _, ok := st.Projects[j.ProjectID]; !ok {
			continue
		}
		if !validID(id) {
			return errors.New("invalid job id")
		}
		if err := put(recordKey(j.ProjectID, "jobs", id), s.state.Jobs[id], j); err != nil {
			return err
		}
	}
	for id, e := range st.Audit {
		if _, ok := st.Projects[e.ProjectID]; !ok {
			continue
		}
		if !validID(id) {
			return errors.New("invalid audit id")
		}
		if err := put(recordKey(e.ProjectID, "audit", id), s.state.Audit[id], e); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) ArchivedJob(ctx context.Context, pid, id string) (model.Job, error) {
	if !validID(pid) || !validID(id) {
		return model.Job{}, errors.New("invalid job identity")
	}
	b, err := s.objects.Get(ctx, recordKey(pid, "jobs", id))
	if err != nil {
		return model.Job{}, err
	}
	var j model.Job
	if err = json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	if j.ID != id || j.ProjectID != pid {
		return j, errors.New("invalid archived job identity")
	}
	return j, nil
}

func recordKey(pid, kind, id string) blob.Key {
	return blob.Key("projects/" + pid + "/.kioku/" + kind + "/" + id + ".json")
}
