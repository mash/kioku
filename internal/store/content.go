package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/model"
)

// Objects avoids an import cycle with the concrete local and S3 implementations.
type Objects interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string) ([]byte, error)
	Delete(context.Context, string) error
	List(context.Context, string) ([]string, error)
}
type fileObjects struct{ dir string }

func (f fileObjects) Put(_ context.Context, key string, b []byte) error {
	path := filepath.Join(f.dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return AtomicWrite(path, b, 0600)
}
func (f fileObjects) Get(_ context.Context, key string) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(key)))
}
func (f fileObjects) Delete(_ context.Context, key string) error {
	err := os.Remove(filepath.Join(f.dir, filepath.FromSlash(key)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (f fileObjects) List(_ context.Context, prefix string) ([]string, error) {
	var out []string
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
			out = append(out, key)
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
		"oauth_clients": st.OAuthClients, "oauth_codes": st.OAuthCodes, "oauth_tokens": st.OAuthTokens, "login_codes": st.LoginCodes,
		"users": st.Users, "projects": projectConfigs(st), "organizations": st.Organizations,
		"agent_tokens": st.AgentTokens, "invites": st.Invites, "sessions": st.Sessions, "oauth": st.OAuth, "devices": st.Devices,
	})
}

// projectContent contains no account configuration, membership or credentials.
type projectContent struct {
	JobsValidAfter          time.Time                    `json:"jobs_valid_after,omitempty"`
	Schema                  int                          `json:"schema"`
	ProjectID               string                       `json:"project_id"`
	SourceVersion           int64                        `json:"source_version"`
	WikiSourceVersion       int64                        `json:"wiki_source_version"`
	SourcesUpdatedAt        time.Time                    `json:"sources_updated_at"`
	WikiUpdatedAt           time.Time                    `json:"wiki_updated_at"`
	NextPutSequence         int64                        `json:"next_put_sequence"`
	NextIntakeSequence      int64                        `json:"next_intake_sequence"`
	ProcessedIntakeSequence int64                        `json:"processed_intake_sequence"`
	NextGenerateAt          time.Time                    `json:"next_generate_at"`
	GenerateIntervalSeconds int64                        `json:"generate_interval_seconds"`
	SourceHeads             map[string]string            `json:"source_heads"`
	SourceHeadSequences     map[string]int64             `json:"source_head_sequences"`
	Incomplete              bool                         `json:"incomplete"`
	Sources                 map[string]model.Source      `json:"sources"`
	Wiki                    *model.Revision              `json:"wiki,omitempty"`
	Jobs                    map[string]model.Job         `json:"jobs"`
	Uploads                 map[string]model.Upload      `json:"uploads"`
	Audit                   map[string]model.Audit       `json:"audit"`
	Intake                  map[string]model.IntakeEntry `json:"intake"`
}

func contentFor(st model.State, pid string) projectContent {
	p := st.Projects[pid]
	c := projectContent{JobsValidAfter: p.JobsValidAfter, Schema: 1, ProjectID: pid, SourceVersion: p.SourceVersion, WikiSourceVersion: p.WikiSourceVersion, SourcesUpdatedAt: p.SourcesUpdatedAt, WikiUpdatedAt: p.WikiUpdatedAt, NextPutSequence: p.NextPutSequence, NextIntakeSequence: p.NextIntakeSequence, ProcessedIntakeSequence: p.ProcessedIntakeSequence, NextGenerateAt: p.NextGenerateAt, GenerateIntervalSeconds: p.GenerateIntervalSeconds, SourceHeads: p.SourceHeads, SourceHeadSequences: p.SourceHeadSequences, Incomplete: p.WikiIncomplete, Sources: map[string]model.Source{}, Jobs: map[string]model.Job{}, Uploads: map[string]model.Upload{}, Audit: map[string]model.Audit{}, Intake: map[string]model.IntakeEntry{}}
	for id, v := range st.Sources {
		if v.ProjectID == pid {
			c.Sources[id] = v
		}
	}
	if current, ok := st.Revisions[p.Revision]; ok {
		c.Wiki = &current
	}
	for id, v := range st.Jobs {
		if v.ProjectID == pid {
			c.Jobs[id] = v
		}
	}
	for id, v := range st.Uploads {
		if v.ProjectID == pid {
			c.Uploads[id] = v
		}
	}
	for id, v := range st.Audit {
		if v.ProjectID == pid {
			c.Audit[id] = v
		}
	}
	for id, v := range st.Intake {
		if v.ProjectID == pid {
			c.Intake[id] = v
		}
	}
	return c
}
func contentKey(pid string) string { return "projects/" + pid + "/.kioku/state.json" }
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
	if c.Schema != 1 || c.ProjectID != pid {
		return errors.New("invalid project content schema or project identity")
	}
	for id, v := range c.Sources {
		if prior, exists := s.state.Sources[id]; exists && prior.ProjectID != pid {
			return errors.New("duplicate source identity across projects")
		}
		if v.ProjectID != pid || id != v.ID || !validID(id) {
			return errors.New("cross-project source in content")
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
	for _, v := range c.Uploads {
		if v.ProjectID != pid {
			return errors.New("cross-project upload in content")
		}
	}
	for _, v := range c.Audit {
		if v.ProjectID != pid {
			return errors.New("cross-project audit in content")
		}
	}
	for _, v := range c.Intake {
		if v.ProjectID != pid {
			return errors.New("cross-project intake in content")
		}
	}
	for _, src := range c.Sources {
		for _, key := range []string{src.OriginalKey, src.TextKey} {
			if key != "" && !strings.HasPrefix(key, "projects/"+pid+"/") {
				return errors.New("cross-project source object reference")
			}
		}
	}
	if c.Wiki != nil {
		for _, key := range c.Wiki.Files {
			if !strings.HasPrefix(key, "projects/"+pid+"/") {
				return errors.New("cross-project wiki object reference")
			}
		}
	}
	p := s.state.Projects[pid]
	p.JobsValidAfter = c.JobsValidAfter
	p.SourceVersion = c.SourceVersion
	p.WikiSourceVersion = c.WikiSourceVersion
	p.SourcesUpdatedAt = c.SourcesUpdatedAt
	p.WikiUpdatedAt = c.WikiUpdatedAt
	p.NextPutSequence = c.NextPutSequence
	p.NextIntakeSequence = c.NextIntakeSequence
	p.ProcessedIntakeSequence = c.ProcessedIntakeSequence
	p.NextGenerateAt = c.NextGenerateAt
	p.GenerateIntervalSeconds = c.GenerateIntervalSeconds
	p.SourceHeads = c.SourceHeads
	p.SourceHeadSequences = c.SourceHeadSequences
	p.Revision = ""
	if c.Wiki != nil {
		p.Revision = c.Wiki.ID
	}
	p.WikiIncomplete = c.Incomplete
	s.state.Projects[pid] = p
	for id, v := range c.Sources {
		s.state.Sources[id] = v
	}
	if c.Wiki != nil {
		s.state.Revisions[c.Wiki.ID] = *c.Wiki
	}
	for id, v := range c.Jobs {
		s.state.Jobs[id] = v
	}
	for id, v := range c.Uploads {
		s.state.Uploads[id] = v
	}
	for id, v := range c.Audit {
		s.state.Audit[id] = v
	}
	for id, v := range c.Intake {
		s.state.Intake[id] = v
	}
	return s.flushIntake(ctx, c)
}
func (s *Store) flushIntake(ctx context.Context, c projectContent) error {
	for id, e := range c.Intake {
		// Immutable IDs make retrying the exact same object idempotent. The writer is
		// fenced by the catalog process lock; no other server writes this project.
		key := fmt.Sprintf("projects/%s/wiki/inbox/%020d-%s.json", c.ProjectID, e.Sequence, id)
		if !validID(id) {
			return errors.New("invalid intake id")
		}
		if s.logged == nil {
			s.logged = map[string]bool{}
		}
		if s.logged[key] {
			continue
		}
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		existing, err := s.objects.Get(ctx, key)
		if err == nil {
			if !bytes.Equal(existing, b) {
				return errors.New("append-only intake entry changed")
			}
		} else if missingObject(err) {
			if err = s.objects.Put(ctx, key, b); err != nil {
				return err
			}
		} else {
			return err
		}
		s.logged[key] = true
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
	return s.flushIntake(ctx, c)
}

type controlIntent struct {
	Control     json.RawMessage `json:"control"`
	Transaction string          `json:"transaction"`
	Projects    []string        `json:"projects"`
}

func transactionKey(pid, id string) string {
	return "projects/" + pid + "/.kioku/transactions/" + id + ".json"
}
func (s *Store) persist(st model.State) error {
	fresh := map[string]model.IntakeEntry{}
	for id, e := range st.Intake {
		if _, ok := s.state.Intake[id]; !ok {
			fresh[id] = e
		}
	}
	flushNew := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for id, e := range fresh {
			if err := s.flushIntake(ctx, projectContent{ProjectID: e.ProjectID, Intake: map[string]model.IntakeEntry{id: e}}); err != nil {
				return err
			}
		}
		if len(s.logged) > 1024 {
			s.logged = map[string]bool{}
		}
		return nil
	}
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
		return flushNew()
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
	return flushNew()
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
		if c.Schema != 1 || c.ProjectID != pid {
			return errors.New("invalid transaction content")
		}
		if err = s.objects.Put(ctx, contentKey(pid), b); err != nil {
			return err
		}
		if err = s.flushIntake(ctx, c); err != nil {
			return err
		}
	}
	st := model.NewState()
	if err := json.Unmarshal(in.Control, &st); err != nil {
		return err
	}
	if err := writeRecords(s.catalog, st, ""); err != nil {
		return err
	}
	for _, pid := range in.Projects {
		_ = s.objects.Delete(ctx, transactionKey(pid, in.Transaction))
	}
	return nil
}

func (s *Store) archiveLegacy() error {
	root := filepath.Join(s.dir, "migration-backup")
	if _, err := os.Stat(filepath.Join(root, "COMPLETE")); err == nil {
		return nil
	}
	if _, err := os.Stat(root); err == nil {
		return errors.New("incomplete legacy migration archive; preserve it and inspect before retry")
	}
	staging, err := os.MkdirTemp(s.dir, ".migration-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	catalog := filepath.Join(staging, "catalog.sqlite")
	if _, err = s.catalog.Exec("VACUUM INTO ?", catalog); err != nil {
		return err
	}
	if err = os.Chmod(catalog, 0600); err != nil {
		return err
	}
	for pid := range s.state.Projects {
		src := filepath.Join(s.dir, "projects", pid, "project.sqlite")
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		db, err := s.projectDB(pid, false)
		if err != nil {
			return err
		}
		dest := filepath.Join(staging, "projects", pid)
		if err = os.MkdirAll(dest, 0700); err != nil {
			db.Close()
			return err
		}
		file := filepath.Join(dest, "project.sqlite")
		_, err = db.Exec("VACUUM INTO ?", file)
		db.Close()
		if err != nil {
			return err
		}
		if err = os.Chmod(file, 0600); err != nil {
			return err
		}
	}
	if err = AtomicWrite(filepath.Join(staging, "COMPLETE"), []byte("legacy archive; content now lives in project storage\n"), 0600); err != nil {
		return err
	}
	return os.Rename(staging, root)
}

func registerBuckets(objects Objects, st model.State) {
	if router, ok := objects.(interface{ RegisterProjectBucket(string, string) }); ok {
		for id, p := range st.Projects {
			router.RegisterProjectBucket(id, p.StorageBucket)
		}
	}
}

const hotRecords = 256
const hotIntake = 64

// The append-only journal and job archive live in R2. Keep only recent records
// in the working index so a busy project's state.json does not grow forever.
func compactContent(st *model.State) {
	for pid := range st.Projects {
		var jobs []model.Job
		for _, j := range st.Jobs {
			if j.ProjectID == pid && j.Status != "running" && j.Status != "queued" && (j.Managed == nil || j.Managed.CleanupComplete) {
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
		var intake []model.IntakeEntry
		for _, e := range st.Intake {
			if e.ProjectID == pid {
				intake = append(intake, e)
			}
		}
		sort.Slice(intake, func(i, j int) bool { return intake[i].Sequence > intake[j].Sequence })
		if len(intake) > hotIntake {
			for _, e := range intake[hotIntake:] {
				delete(st.Intake, e.ID)
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
	put := func(key string, old, record any) error {
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
		if err := put("projects/"+j.ProjectID+"/.kioku/jobs/"+id+".json", s.state.Jobs[id], j); err != nil {
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
		if err := put("projects/"+e.ProjectID+"/.kioku/audit/"+id+".json", s.state.Audit[id], e); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) ArchivedJob(ctx context.Context, pid, id string) (model.Job, error) {
	if !validID(pid) || !validID(id) {
		return model.Job{}, errors.New("invalid job identity")
	}
	b, err := s.objects.Get(ctx, "projects/"+pid+"/.kioku/jobs/"+id+".json")
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
