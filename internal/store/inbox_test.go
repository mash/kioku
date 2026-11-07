package store

import (
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestInboxSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(st *model.State) error {
		st.Projects["p"] = model.Project{ID: "p"}
		st.Inbox["i"] = model.InboxItem{ID: "i", ProjectID: "p", SourceURL: "https://example.com/a", Status: "pending", RunID: "attempt"}
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
		if len(st.Inbox) != 1 || st.Inbox["i"].RunID != "attempt" {
			t.Fatal(st.Inbox)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
