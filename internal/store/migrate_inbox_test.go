package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

func oldInboxFixture(t *testing.T) (string, Objects) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update(func(st *model.State) error {
		st.Users["u"] = model.User{ID: "u", Connections: map[string]string{"google": "encrypted"}}
		st.Projects["p"] = model.Project{ID: "p", Name: "Keep project", Members: map[string]string{"u": "owner"}}
		st.AgentTokens["job"] = model.AgentToken{ID: "job", ProjectID: "p", JobID: "old"}
		st.AgentTokens["human"] = model.AgentToken{ID: "human", ProjectID: "p"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	objects := s.objects
	if _, err = MigrateInbox(t.Context(), dir, objects); err == nil {
		t.Fatal("migration ignored running server lock")
	}
	s.Close()
	raw := `{"schema":1,"project_id":"p","sources":{"s":{"id":"s"}},"uploads":{"u":{}},"intake":{"i":{}},"source_version":12,"source_heads":{"https://example.com":"s"},"wiki":{"id":"r","project_id":"p"},"jobs":{"old":{"id":"old"}},"incomplete":true,"audit":{"a":{"id":"a","project_id":"p"}}}`
	if err = objects.Put(t.Context(), contentKey("p"), []byte(raw)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wiki/index.md", "wiki/inbox/entry.json", "maps/graph.json", "sources/s/text", ".kioku/jobs/archived.json", ".kioku/jobs/old/input.tar.gz", ".kioku/transactions/old.json", ".kioku/inbox/stale.md", ".kioku/audit/a.json"} {
		if err = objects.Put(t.Context(), blob.Key("projects/p/"+name), []byte("old")); err != nil {
			t.Fatal(err)
		}
	}
	return dir, objects
}
func TestMigrateInboxClearsOldContentAndKeepsAccountConfiguration(t *testing.T) {
	dir, objects := oldInboxFixture(t)
	if s, err := OpenWithObjects(dir, objects); err == nil {
		s.Close()
		t.Fatal("old content accepted without migration")
	}
	if n, err := MigrateInbox(t.Context(), dir, objects); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	keys, err := objects.List(t.Context(), "projects/p/")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatal("content survived", keys)
	}
	raw, _ := objects.Get(t.Context(), contentKey("p"))
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	for _, key := range []string{"sources", "uploads", "intake", "source_version", "source_heads", "wiki"} {
		if _, ok := fields[key]; ok {
			t.Fatal("legacy field survived", key)
		}
	}
	s, err := OpenWithObjects(dir, objects)
	if err != nil {
		t.Fatal(err)
	}
	s.View(func(st model.State) error {
		if st.Users["u"].Connections["google"] != "encrypted" || st.Projects["p"].Members["u"] != "owner" || st.Audit["a"].ID != "a" || st.AgentTokens["job"].ID != "" || st.AgentTokens["human"].ID == "" {
			t.Fatal(st)
		}
		return nil
	})
	s.Update(func(st *model.State) error {
		st.Inbox["new"] = model.InboxItem{ID: "new", ProjectID: "p", Status: "pending"}
		st.AgentTokens["newjob"] = model.AgentToken{ID: "newjob", ProjectID: "p", JobID: "new"}
		return nil
	})
	s.Close()
	if n, err := MigrateInbox(t.Context(), dir, objects); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	s, err = OpenWithObjects(dir, objects)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.View(func(st model.State) error {
		if st.Inbox["new"].ID == "" || st.AgentTokens["newjob"].ID == "" {
			t.Fatal("repeat destroyed new data")
		}
		return nil
	})
	if _, err = MigrateInbox(t.Context(), filepath.Join(t.TempDir(), "missing"), objects); err == nil {
		t.Fatal("accepted missing data directory")
	}
}

type interruptMigration struct {
	Objects
	failed bool
}

func (s *interruptMigration) Delete(ctx context.Context, k blob.Key) error {
	if !s.failed && strings.Contains(string(k), "/wiki/") {
		s.failed = true
		return errors.New("interrupted deletion")
	}
	return s.Objects.Delete(ctx, k)
}
func TestMigrateInboxResumesInterruptedDeletion(t *testing.T) {
	dir, objects := oldInboxFixture(t)
	interrupted := &interruptMigration{Objects: objects}
	if _, err := MigrateInbox(t.Context(), dir, interrupted); err == nil {
		t.Fatal("missing interruption")
	}
	if s, err := OpenWithObjects(dir, objects); err == nil {
		s.Close()
		t.Fatal("partial migration enabled server")
	}
	if n, err := MigrateInbox(t.Context(), dir, objects); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
