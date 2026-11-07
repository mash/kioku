package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func TestContentIsolationMigrationAndConfigurationOnlyBackup(t *testing.T) {
	dir := t.TempDir()
	st := model.NewState()
	st.Users["owner"] = model.User{ID: "owner"}
	for _, pid := range []string{"alpha", "beta"} {
		st.Projects[pid] = model.Project{ID: pid, Members: map[string]string{"owner": "owner"}, SourceVersion: 1}
		st.Sources[pid] = model.Source{ID: pid, ProjectID: pid, Title: pid + " secret"}
	}
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range []string{"alpha", "beta"} {
		raw, err = os.ReadFile(filepath.Join(dir, "objects", filepath.FromSlash(string(contentKey(pid)))))
		if err != nil {
			t.Fatal(err)
		}
		var c projectContent
		if err = json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		if len(c.Sources) != 1 || c.Sources[pid].ProjectID != pid || c.SourceVersion != 1 {
			t.Fatal("content isolation or migration failed", c)
		}
		if _, err = os.Stat(filepath.Join(dir, "projects", pid, "project.sqlite")); !os.IsNotExist(err) {
			t.Fatal("project SQLite remains active", err)
		}
	}
	var forbidden int
	if err = s.catalog.QueryRow("SELECT count(*) FROM records WHERE kind IN ('sources','jobs','revisions','uploads','intake','audit')").Scan(&forbidden); err != nil || forbidden != 0 {
		t.Fatal("content in SQLite", forbidden, err)
	}
	rows, err := s.catalog.Query("SELECT payload FROM records WHERE kind='projects'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var b []byte
		rows.Scan(&b)
		var fields map[string]json.RawMessage
		json.Unmarshal(b, &fields)
		for _, key := range []string{"source_version", "source_heads", "revision", "next_generate_at", "wiki_updated_at"} {
			if _, ok := fields[key]; ok {
				t.Error("content checkpoint in SQLite", key)
			}
		}
	}
	rows.Close()
	backup := filepath.Join(t.TempDir(), "backup")
	if err = s.Backup(backup); err != nil {
		t.Fatal(err)
	}
	var changesBefore, changesAfter int64
	s.catalog.QueryRow("SELECT total_changes()").Scan(&changesBefore)
	if err = s.Update(func(st *model.State) error {
		src := st.Sources["alpha"]
		src.Title = "Content newer than SQLite backup"
		st.Sources[src.ID] = src
		p := st.Projects["alpha"]
		p.SourceVersion = 2
		st.Projects[p.ID] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.catalog.QueryRow("SELECT total_changes()").Scan(&changesAfter)
	if changesBefore != changesAfter {
		t.Error("content-only change wrote SQLite", changesBefore, changesAfter)
	}
	s.Close()
	if _, err = os.Stat(filepath.Join(dir, "metadata.json.migrated")); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenWithObjects(backup, fileObjects{dir: filepath.Join(dir, "objects")})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	_ = restored.View(func(st model.State) error {
		if len(st.Sources) != 2 || st.Sources["alpha"].Title != "Content newer than SQLite backup" || st.Projects["alpha"].SourceVersion != 2 || len(st.Users) != 1 {
			t.Fatal("restoring config rolled back content", st.Sources)
		}
		return nil
	})
}

func TestRedoJournalRecoversControlAndExternalContent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := model.NewState()
	st.Projects["p"] = model.Project{ID: "p", Name: "Committed intent"}
	st.Sources["s"] = model.Source{ID: "s", ProjectID: "p", Title: "Recovered"}
	control, _ := controlJSON(st)
	in := controlIntent{Control: control, Transaction: "transaction", Projects: []string{"p"}}
	content, _ := json.Marshal(contentFor(st, "p"))
	if err = s.objects.Put(context.Background(), transactionKey("p", in.Transaction), content); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	if _, err = s.catalog.Exec("INSERT INTO pending(id,payload)VALUES(1,?)", raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("Recovered")) {
		t.Fatal("content leaked to SQLite redo")
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.View(func(st model.State) error {
		if st.Sources["s"].Title != "Recovered" || st.Projects["p"].Name != "Committed intent" {
			t.Fatal("incomplete recovery")
		}
		return nil
	})
	var n int
	if err = s.catalog.QueryRow("SELECT count(*)FROM pending").Scan(&n); err != nil || n != 0 {
		t.Fatal("journal not cleared", err)
	}
}

func TestMissingOrInvalidProjectContentFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing file", "empty file", "wrong project", "unsupported version"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Update(func(st *model.State) error {
				st.Projects["p"] = model.Project{ID: "p"}
				st.Sources["s"] = model.Source{ID: "s", ProjectID: "p"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			file := filepath.Join(dir, "objects", filepath.FromSlash(string(contentKey("p"))))
			switch mode {
			case "missing file":
				err = os.Remove(file)
			case "empty file":
				err = os.WriteFile(file, nil, 0600)
			case "wrong project":
				err = os.WriteFile(file, []byte(`{"schema":1,"project_id":"other"}`), 0600)
			case "unsupported version":
				err = os.WriteFile(file, []byte(`{"schema":99,"project_id":"p"}`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir)
			if err == nil {
				reopened.Close()
				t.Fatal("invalid content silently accepted")
			}
			if mode == "missing file" {
				if _, err = os.Stat(file); !os.IsNotExist(err) {
					t.Fatal("content recreated", err)
				}
			}
		})
	}
}

func TestMissingCatalogWithLocalProjectStorageFailsClosed(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(st *model.State) error { st.Projects["p"] = model.Project{ID: "p"}; return nil }); err != nil {
		t.Fatal(err)
	}
	s.Close()
	file := filepath.Join(dir, "catalog.sqlite")
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err == nil {
		reopened.Close()
		t.Fatal("missing catalog accepted")
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("missing catalog recreated", err)
	}
}

func TestConfigurationBackupDoesNotNeedContentStorage(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Update(func(st *model.State) error { st.Projects["p"] = model.Project{ID: "p"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "objects", filepath.FromSlash(string(contentKey("p"))))); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if err = s.Backup(backup); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenControl(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	_ = restored.View(func(st model.State) error {
		if st.Projects["p"].ID != "p" {
			t.Error("project configuration lost")
		}
		if len(st.Sources) != 0 {
			t.Error("content present in configuration backup")
		}
		return nil
	})
	if _, err = os.Stat(filepath.Join(backup, "projects")); !os.IsNotExist(err) {
		t.Fatal("per-project SQLite in config backup")
	}
}

func TestUploadReservationPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(st *model.State) error {
		st.Projects["p"] = model.Project{ID: "p"}
		st.Uploads["u"] = model.Upload{ID: "u", ProjectID: "p", Sources: []model.Source{{ID: "s", ProjectID: "p", Bytes: 123, TextKey: "projects/p/source/text"}}, Job: model.Job{ID: "j", ProjectID: "p"}, Heads: map[string]string{"https://example.com": "s"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.View(func(st model.State) error {
		u := st.Uploads["u"]
		if len(u.Sources) != 1 || u.Sources[0].Bytes != 123 || u.Heads["https://example.com"] != "s" {
			t.Fatal("pending upload lost on restart")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreLoadsAndKeepsRecordsOfRetiredBilling(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Earlier versions stored these rows for Stripe billing.
	for _, q := range []string{
		`INSERT INTO records(kind,id,payload) VALUES('periods','p','{"id":"p","org_id":"org"}'),('holds','h','{"id":"h","maximum_micros":100}'),('outbox','o','{"id":"o"}'),('alerts','a','{"id":"a"}'),('stripe_events','e','{"id":"e"}')`,
		`INSERT INTO records(kind,id,payload) VALUES('organizations','org','{"id":"org","slug":"acme","name":"Acme","members":{"u":"owner"},"monthly_cap_micros":5,"subscription_status":"inactive","stripe_customer":"cus_old"}')`,
		`CREATE TABLE usage_ledger(id TEXT PRIMARY KEY,payload BLOB NOT NULL)`,
		`INSERT INTO usage_ledger(id,payload) VALUES('usage:h','{"id":"usage:h","kind":"ai_usage"}')`,
		`CREATE TRIGGER usage_ledger_no_delete BEFORE DELETE ON usage_ledger BEGIN SELECT RAISE(ABORT,'usage ledger is append-only'); END`,
	} {
		if _, err = s.catalog.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Update(func(st *model.State) error {
		if st.Organizations["org"].Slug != "acme" {
			t.Error("organization with billing fields did not load", st.Organizations)
		}
		st.Users["u"] = model.User{ID: "u", Name: "User"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var retired, ledger int
	if err = s.catalog.QueryRow("SELECT count(*) FROM records WHERE kind IN " + retiredKinds).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if err = s.catalog.QueryRow("SELECT count(*) FROM usage_ledger").Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if retired != 5 || ledger != 1 {
		t.Fatal("save deleted retired billing records", retired, ledger)
	}
	var org string
	if err = s.catalog.QueryRow("SELECT payload FROM records WHERE kind='organizations' AND id='org'").Scan(&org); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(org, `"stripe_customer":"cus_old"`) || !strings.Contains(org, `"monthly_cap_micros":5`) {
		t.Fatal("save dropped stored organization fields", org)
	}
	if err = s.Update(func(st *model.State) error {
		o := st.Organizations["org"]
		o.Name = "Acme 2"
		st.Organizations["org"] = o
		return nil
	}); err != nil {
		t.Fatal("second save failed", err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if err = s.Backup(backup); err != nil {
		t.Fatal(err)
	}
	b, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.catalog.QueryRow("SELECT count(*) FROM records WHERE kind IN " + retiredKinds).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if err = b.catalog.QueryRow("SELECT count(*) FROM usage_ledger").Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if err = b.catalog.QueryRow("SELECT payload FROM records WHERE kind='organizations' AND id='org'").Scan(&org); err != nil {
		t.Fatal(err)
	}
	if retired != 5 || ledger != 1 || !strings.Contains(org, `"stripe_customer":"cus_old"`) || !strings.Contains(org, `"name":"Acme 2"`) {
		t.Fatal("backup lost retired billing records", retired, ledger, org)
	}
	var job model.Job
	if err = json.Unmarshal([]byte(`{"id":"j","managed":{"hold":{"id":"attempt","org_id":"org","maximum_micros":100,"price":{}}}}`), &job); err != nil || job.Managed.Attempt.ID != "attempt" {
		t.Fatal("stored hold did not decode as the attempt", job.Managed, err)
	}
}

func TestAgentCredentialMetadataSurvivesRestartAndBackup(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	record := model.AgentToken{ID: "delegation", ProjectID: "project", UserID: "user", Name: "Cloud", Role: "read", TokenHash: "synthetic-hash", Created: time.Now().UTC(), Expires: time.Now().UTC().Add(time.Hour)}
	if err = s.Update(func(st *model.State) error { st.AgentTokens[record.ID] = record; return nil }); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if err = s.Backup(backup); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, path := range []string{dir, backup} {
		reopened, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		_ = reopened.View(func(st model.State) error {
			if got := st.AgentTokens[record.ID]; got.TokenHash != record.TokenHash || got.ProjectID != record.ProjectID || !got.Expires.Equal(record.Expires) {
				t.Error("delegation metadata lost", got)
			}
			return nil
		})
		reopened.Close()
	}
}

func TestLegacySQLiteMigrationPreservesCurrentWikiAndArchivesOldFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := model.NewState()
	st.Projects["p"] = model.Project{ID: "p", Revision: "selected", SourceVersion: 7, WikiSourceVersion: 7}
	st.Revisions["selected"] = model.Revision{ID: "selected", ProjectID: "p"}
	st.Revisions["other"] = model.Revision{ID: "other", ProjectID: "p"}
	if err = writeRecords(s.catalog, st, "", true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.catalog.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	project, err := s.projectDB("p", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeRecords(project, st, "p"); err != nil {
		t.Fatal(err)
	}
	project.Close()
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.View(func(st model.State) error {
		if st.Projects["p"].Revision != "selected" || st.Projects["p"].SourceVersion != 7 {
			t.Error("migration lost current content state")
		}
		return nil
	})
	for _, name := range []string{"catalog.sqlite", "projects/p/project.sqlite", "COMPLETE"} {
		if _, err = os.Stat(filepath.Join(dir, "migration-backup", filepath.FromSlash(name))); err != nil {
			t.Fatal("legacy archive incomplete", name, err)
		}
	}
	var version int
	s.catalog.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 2 {
		t.Fatal("migration schema not committed", version)
	}
	if _, err = os.Stat(filepath.Join(dir, "projects", "p", "project.sqlite")); !os.IsNotExist(err) {
		t.Fatal("legacy content SQLite still active")
	}
}

func TestBusyProjectIndexesStayBoundedAndArchivesRemainAvailable(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Update(func(st *model.State) error {
		st.Projects["p"] = model.Project{ID: "p", NextIntakeSequence: 300}
		for i := 1; i <= 300; i++ {
			id := fmt.Sprintf("entry-%03d", i)
			st.Intake[id] = model.IntakeEntry{ID: id, ProjectID: "p", Sequence: int64(i)}
			st.Jobs[id] = model.Job{ID: id, ProjectID: "p", Status: "completed", Created: time.Unix(int64(i), 0)}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.View(func(st model.State) error {
		if len(st.Intake) != hotIntake || len(st.Jobs) != hotRecords {
			t.Error("working index unbounded", len(st.Intake), len(st.Jobs))
		}
		return nil
	})
	keys, err := s.objects.List(context.Background(), "projects/p/wiki/inbox/")
	if err != nil || len(keys) != 300 {
		t.Fatal("append-only entries lost while compacting", len(keys), err)
	}
	job, err := s.ArchivedJob(context.Background(), "p", "entry-001")
	if err != nil || job.ID != "entry-001" {
		t.Fatal("archived job unavailable", job, err)
	}
}

func TestInterruptedLegacyRedoMigratesWithoutLosingWikiSelection(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := model.NewState()
	st.Projects["p"] = model.Project{ID: "p", Revision: "chosen", SourceVersion: 9}
	st.Revisions["chosen"] = model.Revision{ID: "chosen", ProjectID: "p"}
	st.Revisions["other"] = model.Revision{ID: "other", ProjectID: "p"}
	if _, err = s.catalog.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	if _, err = s.catalog.Exec("INSERT INTO pending(id,payload)VALUES(1,?)", raw); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.View(func(st model.State) error {
		if st.Projects["p"].Revision != "chosen" || st.Projects["p"].SourceVersion != 9 {
			t.Error("interrupted migration lost current wiki")
		}
		return nil
	})
	archived, err := openDB(filepath.Join(dir, "migration-backup", "catalog.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer archived.Close()
	var body []byte
	if err = archived.QueryRow("SELECT payload FROM records WHERE kind='projects' AND id='p'").Scan(&body); err != nil {
		t.Fatal(err)
	}
	var p model.Project
	json.Unmarshal(body, &p)
	if p.Revision != "chosen" {
		t.Error("legacy archive lost original selection")
	}
}
