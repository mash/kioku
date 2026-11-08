package server

import (
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestProjectsIncludeOnlyTheirInboxSummary(t *testing.T) {
	f := setupWithWorker(t, false)
	owner := f.login("owner@example.com")
	p := securityProject(t, f, owner)
	var empty model.Project
	if c := f.request("POST", "/api/projects", owner, map[string]string{"name": "Empty"}, &empty); c != 201 {
		t.Fatal(c)
	}
	other := f.login("other@example.com")
	private := securityProject(t, f, other)
	if err := f.db.Update(func(st *model.State) error {
		st.Inbox["one"] = model.InboxItem{ID: "one", ProjectID: p.ID, SourceURL: "https://example.com/a", Status: "fetched", RunID: "pinned", Submitter: "owner"}
		st.Inbox["two"] = model.InboxItem{ID: "two", ProjectID: p.ID, SourceURL: "https://example.com/b", Status: "fetched"}
		st.Inbox["private"] = model.InboxItem{ID: "private", ProjectID: private.ID, SourceURL: "https://example.com/private", Status: "removed"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, agent := testAgent(t, f, owner, p.ID, "read")
	for _, tc := range []struct {
		name, token string
		count       int
	}{{"owner", owner, 2}, {"agent", agent, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			var entries []struct {
				ID         string              `json:"id"`
				InboxCount int                 `json:"inbox_count"`
				Inbox      []map[string]string `json:"inbox"`
			}
			if c := f.request("GET", "/api/projects", tc.token, nil, &entries); c != 200 || len(entries) != tc.count {
				t.Fatal(c, entries)
			}
			for _, entry := range entries {
				switch entry.ID {
				case p.ID:
					if entry.InboxCount != 2 || len(entry.Inbox) != 2 {
						t.Fatal(entry)
					}
					for i, item := range entry.Inbox {
						wantURL, wantStatus := "https://example.com/a", "fetched"
						if i == 1 {
							wantURL, wantStatus = "https://example.com/b", "fetched"
						}
						if len(item) != 2 || item["source_url"] != wantURL || item["status"] != wantStatus {
							t.Fatal(item)
						}
					}
				case empty.ID:
					if entry.InboxCount != 0 || entry.Inbox == nil || len(entry.Inbox) != 0 {
						t.Fatal(entry)
					}
				default:
					t.Fatal("inaccessible project exposed", entry)
				}
			}
			want := 404
			if tc.name == "agent" {
				want = 403 // Unmatched routes reach the web handler, forbidden to agents.
			}
			if c := f.request("GET", "/api/projects/"+p.ID+"/inbox", tc.token, nil, nil); c != want {
				t.Fatal("removed endpoint remains available", c)
			}
		})
	}
}
