package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestBackupCapturesAllTablesInOneSnapshot(t *testing.T) {
	d := newCatalog(t)
	catalogWrite(t, d, func(q *Queries) error {
		writes := []func() error{
			func() error { return q.PutOrganization(OrganizationRow{ID: "o", Slug: "org"}) },
			func() error {
				return q.PutUser(UserRow{ID: "u", Email: "user@example.com", Data: UserData{Name: "before"}})
			},
			func() error { return q.PutOrganizationMember("o", "u", "owner") },
			func() error { return q.PutIdentity("u", "github", "external") },
			func() error { return q.PutProject(ProjectRow{ID: "p", OrgID: "o", Slug: "project"}) },
			func() error { return q.PutProjectMember("p", "u", "owner") },
			func() error {
				return q.InsertSession(SessionRow{ID: "s", Kind: "web", Status: "active", UserID: "u", Data: json.RawMessage(`{}`)}, 0)
			},
			func() error { return q.PutInvite(InviteRow{ID: "i", ProjectID: "p", CreatedBy: "u", TokenHash: "h"}) },
			func() error { return q.PutOAuthClient(OAuthClientRow{ClientID: "c"}) },
			func() error { return q.PutInbox(InboxRow{ID: "i", ProjectID: "p", SourceURL: "s"}) },
			func() error {
				return q.PutWikiFile(WikiFileRow{ProjectID: "p", Path: "wiki/index.md", Hash: strings.Repeat("a", 64)})
			},
			func() error { return q.PutJob(testJob("j", "p", "completed", 1000)) },
			func() error { return q.PutAudit(AuditRow{ID: "a", ProjectID: "p"}) },
			func() error { return q.PutView(ViewRow{ID: "v", ProjectID: "p", Slug: "wiki"}) },
			func() error { return q.RecordUsage(UsageRow{AttemptID: "j", ProjectID: "p", JobID: "j"}) },
		}
		for _, write := range writes {
			if e := write(); e != nil {
				return e
			}
		}
		return nil
	})
	tables := []string{"organizations", "organization_members", "users", "user_identities", "projects", "project_members", "sessions", "invites", "oauth_clients", "inbox", "wiki_files", "jobs", "audit", "views", "usage"}
	dest := filepath.Join(t.TempDir(), "snapshot")
	if err := d.backup(t.Context(), dest, func() error {
		return d.Write(t.Context(), func(q *Queries) error {
			for _, table := range tables {
				if _, e := q.exec("DELETE FROM " + table); e != nil {
					return e
				}
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenDatabase(filepath.Join(dest, "catalog.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, table := range append(tables, "schema_state") {
		var n int
		if e := restored.reader.QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil || n != 1 {
			t.Fatal(table, n, e)
		}
	}
	if e := restored.Read(t.Context(), func(q *Queries) error {
		u, e := q.User("u")
		if u.Data.Name != "before" {
			t.Fatal(u)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	// The original writer stayed usable throughout the dedicated read snapshot.
	catalogWrite(t, d, func(q *Queries) error { return q.PutUser(UserRow{ID: "later", Email: "later"}) })
	manifest, err := os.ReadFile(filepath.Join(dest, "backup.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err = json.Unmarshal(manifest, &meta); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) != 4 || meta["schema"] != float64(3) || meta["content_included"] != false || meta["sqlite_sha256"] != string(model.HashContent(body)) {
		t.Fatal(meta)
	}
	for _, path := range []string{dest, filepath.Join(dest, "catalog.sqlite"), filepath.Join(dest, "backup.json")} {
		info, e := os.Stat(path)
		want := os.FileMode(0600)
		if path == dest {
			want = 0700
		}
		if e != nil || info.Mode().Perm() != want {
			t.Fatal(path, e)
		}
	}
	if err = d.Backup(dest); err == nil {
		t.Fatal("overwrote existing backup")
	}
}
func TestBackupRejectsUnreadyCatalog(t *testing.T) {
	d := newCatalog(t)
	if _, err := d.writer.Exec(`UPDATE schema_state SET data='{"ready":false}'`); err != nil {
		t.Fatal(err)
	}
	if err := d.Backup(filepath.Join(t.TempDir(), "backup")); err == nil {
		t.Fatal("backed up incomplete migration")
	}
}
