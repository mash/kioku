package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

// Migration fixtures have no network path. All legacy inputs are explicitly
// supplied, including absent objects, interrupted writes and old credentials.
type migrationObjects struct {
	values        map[blob.Key][]byte
	puts, deletes int
	beforePut     func(blob.Key) error
	beforeDelete  func(blob.Key) error
}

func (m *migrationObjects) Get(_ context.Context, k blob.Key) ([]byte, error) {
	v, ok := m.values[k]
	if !ok {
		return nil, os.ErrNotExist
	}
	return bytes.Clone(v), nil
}
func (m *migrationObjects) Put(_ context.Context, k blob.Key, v []byte) error {
	m.puts++
	if m.beforePut != nil {
		if e := m.beforePut(k); e != nil {
			return e
		}
	}
	m.values[k] = bytes.Clone(v)
	return nil
}
func (m *migrationObjects) Delete(_ context.Context, k blob.Key) error {
	m.deletes++
	if m.beforeDelete != nil {
		if e := m.beforeDelete(k); e != nil {
			return e
		}
	}
	delete(m.values, k)
	return nil
}
func (m *migrationObjects) List(_ context.Context, prefix string) ([]blob.Key, error) {
	keys := []blob.Key{}
	for key := range m.values {
		if strings.HasPrefix(string(key), prefix) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys, nil
}
func (m *migrationObjects) set(t *testing.T, k blob.Key, v any) {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	m.values[k] = b
}
func migrationRecord(t *testing.T, path, kind, id string, v any) {
	t.Helper()
	db, e := catalogConnection(path, false, false)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO records(kind,id,payload) VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET payload=excluded.payload", kind, id, b); e != nil {
		t.Fatal(e)
	}
}
func migrationFixture(t *testing.T) (string, *migrationObjects, *auth.Cipher, legacyV2Content) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	db, e := catalogConnection(path, true, false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`CREATE TABLE records(kind TEXT NOT NULL,id TEXT NOT NULL,payload BLOB NOT NULL,PRIMARY KEY(kind,id));CREATE TABLE pending(id INTEGER PRIMARY KEY CHECK(id=1),payload BLOB NOT NULL);CREATE TABLE usage_ledger(id TEXT PRIMARY KEY,payload BLOB);PRAGMA user_version=2`); e != nil {
		t.Fatal(e)
	}
	db.Close()
	now := time.UnixMilli(1000).UTC()
	migrationRecord(t, path, "organizations", "o", legacyV2Organization{ID: "o", Slug: "org", Members: map[string]string{"u": "owner"}, Created: now})
	migrationRecord(t, path, "users", "u", legacyV2User{ID: "u", Name: "User", Email: "u@example.com", Identities: map[string]string{"github": "external"}})
	migrationRecord(t, path, "projects", "p", legacyV2Project{ID: "p", OrgID: "o", Slug: "project", Namespace: "org/project", Members: map[string]string{"u": "owner"}, Created: now, StorageRoot: "projects/p/"})
	objects := &migrationObjects{values: map[blob.Key][]byte{}}
	var content legacyV2Content
	fixture, e := os.ReadFile("testdata/migration/empty-state-v2.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(fixture, &content); e != nil {
		t.Fatal(e)
	}
	objects.set(t, "projects/p/.kioku/state.json", content)
	cipher, e := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if e != nil {
		t.Fatal(e)
	}
	return path, objects, cipher, content
}
func TestMigrationValidatesEverythingBeforeDelete(t *testing.T) {
	for _, failure := range []string{"missing body", "hash mismatch", "corrupt view", "bad identity"} {
		t.Run(failure, func(t *testing.T) {
			path, b, c, content := migrationFixture(t)
			content.Wiki = &legacyV2Revision{ID: "w", ProjectID: "p", Files: map[string]blob.Key{"wiki/index.md": "projects/p/wiki/index.md"}, FileHashes: map[string]string{}, FileBytes: map[string]int64{}}
			b.values["projects/p/wiki/index.md"] = []byte("wiki")
			switch failure {
			case "missing body":
				content.Inbox["i"] = legacyV2InboxItem{ID: "i", ProjectID: "p", SourceURL: "s"}
			case "hash mismatch":
				content.Wiki.FileHashes["wiki/index.md"] = strings.Repeat("0", 64)
			case "corrupt view":
				b.values["projects/p/.kioku/views/index.json"] = []byte("{")
			case "bad identity":
				migrationRecord(t, path, "sessions", "s", legacyV2Session{UserID: "u", Expires: time.Unix(100000, 0), IdentityProvider: "github", IdentityID: "external", IdentityCredential: "broken secret"})
			}
			b.set(t, "projects/p/.kioku/state.json", content)
			if e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000)); e == nil {
				t.Fatal("accepted invalid migration")
			}
			if b.puts != 0 || b.deletes != 0 {
				t.Fatal("mutation before complete validation", b.puts, b.deletes)
			}
			db, e := catalogConnection(path, false, true)
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			var version int
			if e = db.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != 2 {
				t.Fatal(version, e)
			}
		})
	}
}
func TestMigrationRejectsPendingAndDeletingProjects(t *testing.T) {
	for _, failure := range []string{"pending", "deleting", "running", "cleanup"} {
		t.Run(failure, func(t *testing.T) {
			path, b, c, content := migrationFixture(t)
			switch failure {
			case "pending":
				db, e := catalogConnection(path, false, false)
				if e != nil {
					t.Fatal(e)
				}
				_, e = db.Exec(`INSERT INTO pending VALUES(1,'{}')`)
				db.Close()
				if e != nil {
					t.Fatal(e)
				}
			case "deleting":
				migrationRecord(t, path, "projects", "p", legacyV2Project{ID: "p", OrgID: "o", Slug: "project", Deleting: true})
			case "running":
				content.Jobs["j"] = legacyV2Job{ID: "j", ProjectID: "p", Status: "running"}
			case "cleanup":
				content.Jobs["j"] = legacyV2Job{ID: "j", ProjectID: "p", Status: "completed", Run: &legacyV2AgentRun{Phase: "finalizing"}}
			}
			b.set(t, "projects/p/.kioku/state.json", content)
			if e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000)); e == nil {
				t.Fatal("accepted unfinished work")
			}
			if b.puts != 0 || b.deletes != 0 {
				t.Fatal(b.puts, b.deletes)
			}
		})
	}
}
func TestMigrationKeepsUsedAuthenticationAndDropsJobRows(t *testing.T) {
	path, b, c, _ := migrationFixture(t)
	expires := time.Unix(100000, 0)
	migrationRecord(t, path, "sessions", "human", legacyV2Session{UserID: "u", Expires: expires})
	migrationRecord(t, path, "sessions", "job", legacyV2Session{UserID: "u", Expires: expires, Role: "job"})
	migrationRecord(t, path, "agent_tokens", "a", legacyV2AgentToken{ID: "a", UserID: "u", ProjectID: "p", Name: "agent", Role: "write", TokenHash: "agenthash", Expires: expires})
	migrationRecord(t, path, "agent_tokens", "j", legacyV2AgentToken{ID: "j", UserID: "u", ProjectID: "p", JobID: "job", TokenHash: "jobhash", Expires: expires})
	migrationRecord(t, path, "devices", "device", legacyV2Device{Used: true, UserID: "u", Expires: expires, SecretHash: "discard"})
	migrationRecord(t, path, "oauth", "old-state", legacyV2OAuthState{Provider: "github", Expires: expires, Verifier: "discard"})
	request := json.RawMessage(`{"id":"r","requestedAt":"2026-01-01T00:00:00Z","client":{"id":"c","redirect_uris":[],"grant_types":[],"response_types":[],"scopes":[],"audience":[]},"scopes":[],"grantedScopes":[],"requestedAudience":[],"grantedAudience":[],"form":{},"session":{"expires_at":{},"username":"","subject":"u","project_ids":["p"]}}`)
	migrationRecord(t, path, "oauth_codes", "code", legacyV2OAuthCode{UserID: "u", Active: false, Expires: expires, Request: request, ProjectIDs: []string{"p"}})
	migrationRecord(t, path, "oauth_tokens", "refresh", legacyV2OAuthGrant{UserID: "u", Kind: "refresh", Active: false, Expires: expires, Request: request, ClientID: "c", RequestID: "r", ProjectIDs: []string{"p"}})
	if e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000)); e != nil {
		t.Fatal(e)
	}
	d, e := OpenDatabase(path, false)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if e = d.Read(t.Context(), func(q *Queries) error {
		for id, kind := range map[string]string{"human": "web", "agenthash": "agent", auth.Hash("device"): "cli_login", "code": "oauth_code", "refresh": "mcp"} {
			s, e := q.Session(id)
			if e != nil {
				return e
			}
			if s.Kind != kind || s.Status != "active" {
				t.Fatal(id, s)
			}
		}
		for _, id := range []string{"job", "jobhash", "old-state"} {
			if _, e := q.Session(id); !errors.Is(e, sql.ErrNoRows) {
				t.Fatal(id, e)
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
func TestMigrationDecryptsAndSplitsIdentityTokens(t *testing.T) {
	path, b, c, _ := migrationFixture(t)
	plain := `{"access_token":"private-access","refresh_token":"private-refresh","expires_at":"2026-01-01T00:00:00.123456789Z"}`
	encrypted := c.Seal(plain, "identity:u:github:external")
	migrationRecord(t, path, "sessions", "s", legacyV2Session{UserID: "u", Expires: time.Unix(100000, 0), IdentityProvider: "github", IdentityID: "external", IdentityCredential: encrypted})
	if e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000)); e != nil {
		t.Fatal(e)
	}
	d, e := OpenDatabase(path, false)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if e = d.Read(t.Context(), func(q *Queries) error {
		s, e := q.Session("s")
		if e != nil {
			return e
		}
		var data IdentitySessionData
		if e = json.Unmarshal(s.Data, &data); e != nil {
			return e
		}
		value, e := c.OpenIdentityTokens(auth.IdentityTokens{AccessToken: data.IdentityAccessToken, RefreshToken: data.IdentityRefreshToken, ExpiresAt: data.IdentityExpiresAt}, "u", "github", "external")
		if e != nil {
			return e
		}
		if value.AccessToken != "private-access" || value.RefreshToken != "private-refresh" || value.ExpiresAt.Nanosecond() != 123000000 || bytes.Contains(s.Data, []byte("private-")) {
			t.Fatal("credential migration mismatch")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
func TestMigrationUsageSurvivesHistoryCompaction(t *testing.T) {
	path, b, c, content := migrationFixture(t)
	for i := range 270 {
		id := fmt.Sprintf("j%03d", i)
		cost := int64(i)
		content.Jobs[id] = legacyV2Job{ID: id, ProjectID: "p", UserID: "deleted-user", Status: "completed", Kind: "generate", Created: time.UnixMilli(int64(i)), Run: &legacyV2AgentRun{Phase: "finalizing", CleanupComplete: true, CostCents: &cost, PreparedAt: time.UnixMilli(1000), Deadline: time.UnixMilli(2000), Attempt: legacyV2Attempt{ID: "old-attempt-" + id}}}
	}
	b.set(t, "projects/p/.kioku/state.json", content)
	if e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000)); e != nil {
		t.Fatal(e)
	}
	d, e := OpenDatabase(path, false)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if e = d.Read(t.Context(), func(q *Queries) error {
		jobs, e := q.ProjectJobs("p")
		if e != nil {
			return e
		}
		usage, e := q.ProjectUsage("p")
		if e != nil {
			return e
		}
		if len(jobs) != 256 || len(usage) != 270 {
			t.Fatal(len(jobs), len(usage))
		}
		for _, u := range usage {
			if u.AttemptID != u.JobID || u.RecordedAt != 5000 || u.Data.UserID != "deleted-user" || u.Data.Model != nil || u.Data.CostCents == nil {
				t.Fatal(u)
			}
		}
		for _, j := range jobs {
			if j.InputReady || j.CleanupComplete == nil || !*j.CleanupComplete {
				t.Fatal(j)
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
func TestMigrationKeepsReadyFalseAfterDeleteFailure(t *testing.T) {
	path, b, c, _ := migrationFixture(t)
	b.beforeDelete = func(blob.Key) error { return errors.New("delete failed") }
	if e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000)); e == nil {
		t.Fatal("ignored delete error")
	}
	if _, e := OpenDatabase(path, false); e == nil || !strings.Contains(e.Error(), "migration incomplete") {
		t.Fatal(e)
	}
	deletes := b.deletes
	b.beforeDelete = nil
	if e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000)); e == nil {
		t.Fatal("resumed incomplete migration")
	}
	if b.deletes != deletes {
		t.Fatal("automatically resumed deletion")
	}
}

func TestMigrationPutsBeforeSingleSQLCommitAndDeletesAfterReferences(t *testing.T) {
	path, b, c, content := migrationFixture(t)
	body := []byte("# Wiki\n")
	hash := string(model.HashContent(body))
	content.Wiki = &legacyV2Revision{ID: "w", ProjectID: "p", Created: time.UnixMilli(1001), Files: map[string]blob.Key{"wiki/index.md": "projects/p/wiki/index.md"}, FileBytes: map[string]int64{"wiki/index.md": int64(len(body))}, FileHashes: map[string]string{"wiki/index.md": hash}, StoredBytes: int64(len(body))}
	b.values["projects/p/wiki/index.md"] = body
	b.set(t, "projects/p/.kioku/state.json", content)
	b.beforePut = func(blob.Key) error {
		db, e := catalogConnection(path, false, true)
		if e != nil {
			return e
		}
		defer db.Close()
		var version int
		if e = db.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
			return e
		}
		if version != 2 {
			t.Fatal("SQL committed before PUT", version)
		}
		return nil
	}
	b.beforeDelete = func(blob.Key) error {
		db, e := catalogConnection(path, false, true)
		if e != nil {
			return e
		}
		defer db.Close()
		var version int
		var state, stored string
		if e = db.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
			return e
		}
		if e = db.QueryRow("SELECT data FROM schema_state").Scan(&state); e != nil {
			return e
		}
		if e = db.QueryRow("SELECT hash FROM wiki_files WHERE project_id='p' AND path='wiki/index.md'").Scan(&stored); e != nil {
			return e
		}
		if version != 3 || state != `{"ready":false}` || stored != hash {
			t.Fatal("deletion preceded reference commit", version, state, stored)
		}
		if !bytes.Equal(b.values[model.WikiContentKey("p", model.ContentHash(hash))], body) {
			t.Fatal("missing immutable Wiki")
		}
		return nil
	}
	if e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000)); e != nil {
		t.Fatal(e)
	}
	if b.puts != 1 || b.deletes != 2 {
		t.Fatal(b.puts, b.deletes)
	}
	d, e := OpenDatabase(path, false)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if _, ok := b.values["projects/p/wiki/index.md"]; ok {
		t.Fatal("old page retained")
	}
}
func TestMigrationRejectsSessionKeyCollisionBeforeMutation(t *testing.T) {
	path, b, c, _ := migrationFixture(t)
	migrationRecord(t, path, "sessions", "same", legacyV2Session{UserID: "u"})
	migrationRecord(t, path, "agent_tokens", "agent", legacyV2AgentToken{ID: "agent", UserID: "u", ProjectID: "p", TokenHash: "same", Role: "read"})
	if e := migrateV2(t.Context(), path, b, c, time.UnixMilli(5000)); e == nil {
		t.Fatal("accepted colliding session IDs")
	}
	if b.puts != 0 || b.deletes != 0 {
		t.Fatal(b.puts, b.deletes)
	}
}

func TestMigrationTimePreservesNullAndTruncatesSubMilliseconds(t *testing.T) {
	if migrationTime(time.Time{}) != nil {
		t.Fatal("zero must be null")
	}
	for _, ms := range []int64{-1234, 0, 1794712365123} {
		original := time.UnixMilli(ms).Add(456789 * time.Nanosecond)
		stored := migrationTime(original)
		if stored == nil || *stored != ms {
			t.Fatalf("timestamp: %v", stored)
		}
		b, err := json.Marshal(stored)
		if err != nil || string(b) != strconv.FormatInt(ms, 10) {
			t.Fatalf("JSON: %s %v", b, err)
		}
	}
}
