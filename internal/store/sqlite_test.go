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

func TestRedoJournalRecoversControlAndExternalContent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := model.NewState()
	st.Projects["p"] = model.Project{ID: "p", Name: "Committed intent"}
	st.Inbox["s"] = model.InboxItem{ID: "s", ProjectID: "p", Remove: true}
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
		if !st.Inbox["s"].Remove || st.Projects["p"].Name != "Committed intent" {
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
	for _, mode := range []string{"missing file", "empty file", "wrong project", "legacy content", "unsupported version"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Update(func(st *model.State) error {
				st.Projects["p"] = model.Project{ID: "p"}
				st.Inbox["s"] = model.InboxItem{ID: "s", ProjectID: "p"}
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
				err = os.WriteFile(file, []byte(`{"schema":2,"project_id":"other"}`), 0600)
			case "legacy content":
				err = os.WriteFile(file, []byte(`{"schema":1,"project_id":"p"}`), 0600)
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
			if mode == "legacy content" {
				if !strings.Contains(err.Error(), "old project content (schema 1) must be deleted") {
					t.Fatalf("missing operator guidance: %v", err)
				}
				raw, readErr := os.ReadFile(file)
				if readErr != nil || string(raw) != `{"schema":1,"project_id":"p"}` {
					t.Fatalf("startup modified old content: %s, %v", raw, readErr)
				}
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
		if len(st.Inbox) != 0 {
			t.Error("content present in configuration backup")
		}
		return nil
	})
	if _, err = os.Stat(filepath.Join(backup, "projects")); !os.IsNotExist(err) {
		t.Fatal("per-project SQLite in config backup")
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
	if err = json.Unmarshal([]byte(`{"id":"j","run":{"attempt":{"id":"attempt","org_id":"org","maximum_micros":100,"price":{}}}}`), &job); err != nil || job.Run.Attempt.ID != "attempt" {
		t.Fatal("stored hold did not decode as the attempt", job.Run, err)
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

func TestBusyProjectIndexesStayBoundedAndArchivesRemainAvailable(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Update(func(st *model.State) error {
		st.Projects["p"] = model.Project{ID: "p"}
		for i := 1; i <= 300; i++ {
			id := fmt.Sprintf("entry-%03d", i)
			st.Jobs[id] = model.Job{ID: id, ProjectID: "p", Status: "completed", Created: time.Unix(int64(i), 0)}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.View(func(st model.State) error {
		if len(st.Jobs) != hotRecords {
			t.Error("working index unbounded", len(st.Jobs))
		}
		return nil
	})
	keys, err := s.objects.List(context.Background(), "projects/p/.kioku/jobs/")
	if err != nil || len(keys) != 300 {
		t.Fatal("archived jobs lost while compacting", len(keys), err)
	}
	job, err := s.ArchivedJob(context.Background(), "p", "entry-001")
	if err != nil || job.ID != "entry-001" {
		t.Fatal("archived job unavailable", job, err)
	}
}
