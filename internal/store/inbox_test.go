package store

import (
	"encoding/json"
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
		st.Inbox["i"] = model.InboxItem{ID: "i", ProjectID: "p", SourceURL: "https://example.com/a", RunID: "attempt"}
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

func TestLoadInboxRemoveFlag(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		remove       bool
	}{
		{"fetched", `"status":"fetched"`, false},
		{"removed", `"status":"removed"`, true},
		{"file", `"remove":false`, false},
		{"remove", `"remove":true`, true},
		{"omitted", `"submitter":"u"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Update(func(st *model.State) error {
				st.Projects["p"] = model.Project{ID: "p"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			objects := s.objects
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"schema":2,"project_id":"p","inbox":{"i":{"id":"i","project_id":"p","source_url":"https://example.com/a","run_id":"attempt",` + tc.fields + `}}}`)
			if err := objects.Put(t.Context(), contentKey("p"), body); err != nil {
				t.Fatal(err)
			}
			s, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.Update(func(st *model.State) error {
				item := st.Inbox["i"]
				if item.Remove != tc.remove || item.SourceURL != "https://example.com/a" || item.RunID != "attempt" {
					t.Fatal(item)
				}
				item.Submitter = "updated"
				st.Inbox["i"] = item
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			body, err = objects.Get(t.Context(), contentKey("p"))
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Inbox map[string]map[string]any `json:"inbox"`
			}
			if err := json.Unmarshal(body, &saved); err != nil {
				t.Fatal(err)
			}
			item := saved.Inbox["i"]
			if _, ok := item["status"]; ok {
				t.Fatal("wrote legacy status", string(body))
			}
			if value, ok := item["remove"]; ok != tc.remove || ok && value != true {
				t.Fatal("incorrect remove flag", string(body))
			}
		})
	}
}
