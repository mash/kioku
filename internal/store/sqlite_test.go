package store

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
)

// E1/E17: SQLite owns metadata; a stopped migration is the only old reader.
func TestMissingOrInvalidLegacyProjectContentFailsMigration(t *testing.T) {
	for _, body := range []string{"missing", "", `{"schema":2,"project_id":"other"}`, `{"schema":1,"project_id":"p"}`, `{"schema":99,"project_id":"p"}`} {
		t.Run(body, func(t *testing.T) {
			path, objects, key, _ := migrationFixture(t)
			if body == "missing" {
				delete(objects.values, "projects/p/.kioku/state.json")
			} else {
				objects.values["projects/p/.kioku/state.json"] = []byte(body)
			}
			if e := migrateV2(t.Context(), path, objects, key, time.UnixMilli(1000)); e == nil {
				t.Fatal("invalid old content accepted")
			}
			if objects.puts != 0 || objects.deletes != 0 {
				t.Fatal("validation mutated objects", objects.puts, objects.deletes)
			}
		})
	}
}
func TestRuntimeDoesNotReadLegacyProjectContent(t *testing.T) {
	d := newCatalog(t)
	catalogWrite(t, d, func(q *Queries) error {
		return q.PutProject(ProjectRow{ID: "p", Slug: "project", Data: ProjectData{Name: "SQL name"}})
	})
	path := d.path
	d.Close()
	key, e := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if e != nil {
		t.Fatal(e)
	}
	objects := &migrationObjects{values: map[blob.Key][]byte{"projects/p/.kioku/state.json": []byte("corrupt obsolete object")}}
	restored, _, e := OpenStorage(t.Context(), filepath.Dir(path), objects, func(bool) (*auth.Cipher, error) { return key, nil })
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	if e = restored.Read(t.Context(), func(q *Queries) error {
		p, e := q.Project("p")
		if p.Data.Name != "SQL name" {
			t.Fatal(p)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if objects.deletes != 0 || objects.puts != 0 {
		t.Fatal("normal open touched objects")
	}
}
func TestBackupDoesNotNeedContentStorage(t *testing.T) {
	d := newCatalog(t)
	catalogWrite(t, d, func(q *Queries) error {
		if e := q.PutProject(ProjectRow{ID: "p", Slug: "p"}); e != nil {
			return e
		}
		return q.PutInbox(InboxRow{ID: "i", ProjectID: "p", SourceURL: "https://example.com/a"})
	})
	dest := filepath.Join(t.TempDir(), "backup")
	if e := d.Backup(dest); e != nil {
		t.Fatal(e)
	}
	restored, e := OpenBackupDatabase(dest)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	if e = restored.Read(t.Context(), func(q *Queries) error { _, e := q.Inbox("i"); return e }); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(dest, "objects")); !os.IsNotExist(e) {
		t.Fatal("backup included objects", e)
	}
}
func TestAgentCredentialMetadataSurvivesRestartAndBackup(t *testing.T) {
	d := newCatalog(t)
	now := time.Now().UnixMilli()
	raw, e := json.Marshal(AgentSessionData{AgentID: pointer("delegation"), Name: "Cloud", Role: pointer("read")})
	if e != nil {
		t.Fatal(e)
	}
	record := SessionRow{ID: auth.Hash("synthetic-token"), Kind: "agent", Status: "active", ProjectID: pointer("project"), UserID: "user", Created: now, Expires: now + 3600000, Data: raw}
	catalogWrite(t, d, func(q *Queries) error { return q.PutSession(record) })
	backup := filepath.Join(t.TempDir(), "backup")
	if e = d.Backup(backup); e != nil {
		t.Fatal(e)
	}
	path := d.path
	d.Close()
	for _, file := range []string{path, filepath.Join(backup, "catalog.sqlite")} {
		restored, e := OpenDatabase(file, false)
		if e != nil {
			t.Fatal(e)
		}
		if e = restored.Read(t.Context(), func(q *Queries) error {
			row, e := q.Session(record.ID)
			if !reflect.DeepEqual(record, row) {
				t.Fatal(row, record)
			}
			return e
		}); e != nil {
			t.Fatal(e)
		}
		restored.Close()
	}
}
func TestBusyProjectHistoryHasNoArchiveReader(t *testing.T) {
	d := newCatalog(t)
	catalogWrite(t, d, func(q *Queries) error {
		for i := range 300 {
			if e := q.PutJob(testJob(fmt.Sprintf("entry-%03d", i), "p", "completed", int64(i))); e != nil {
				return e
			}
		}
		return q.CompactHistory("p", nil)
	})
	if e := d.Read(t.Context(), func(q *Queries) error {
		jobs, e := q.ProjectJobs("p")
		if len(jobs) != 256 {
			t.Fatal(len(jobs))
		}
		_, e = q.Job("entry-000")
		if !errors.Is(e, sql.ErrNoRows) {
			t.Fatal("expired history remained", e)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
