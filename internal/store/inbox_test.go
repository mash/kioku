package store

import (
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
)

func TestInboxSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	updatedAt := time.Date(2026, 11, 7, 18, 0, 0, 0, time.FixedZone("", 9*60*60))
	if err = s.Update(func(st *model.State) error {
		st.Projects["p"] = model.Project{ID: "p"}
		st.Inbox["new"] = model.InboxItem{ID: "new", ProjectID: "p", SourceURL: "https://example.com/new", Status: "fetched", UpdatedAt: &updatedAt}
		st.Inbox["i"] = model.InboxItem{ID: "i", ProjectID: "p", SourceURL: "https://example.com/a", Status: "fetched", RunID: "attempt"}
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
		if len(st.Inbox) != 2 || st.Inbox["i"].RunID != "attempt" || st.Inbox["i"].UpdatedAt != nil || st.Inbox["new"].UpdatedAt == nil || st.Inbox["new"].UpdatedAt.Format(time.RFC3339) != updatedAt.Format(time.RFC3339) {
			t.Fatal(st.Inbox)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
