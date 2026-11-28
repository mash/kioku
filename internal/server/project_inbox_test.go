package server

import (
	"context"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func TestProjectsIncludeOnlyTheirInboxSummary(t *testing.T) {
	t.Parallel()
	f := setupWithWorker(t, false)
	owner := f.login("owner@example.com")
	p := securityProject(t, f, owner)
	var empty fixtureProject
	if c := f.request("POST", "/api/projects", owner, map[string]string{"name": "Empty"}, &empty); c != 201 {
		t.Fatal(c)
	}
	other := f.login("other@example.com")
	private := securityProject(t, f, other)
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		fixtureInboxPut(t, st, "one", model.InboxItem{ID: "one", ProjectID: p.ID, SourceURL: "https://example.com/a", RunID: "pinned", Submitter: "owner"})
		fixtureInboxPut(t, st, "two", model.InboxItem{ID: "two", ProjectID: p.ID, SourceURL: "https://example.com/b", Remove: true})
		fixtureInboxPut(t, st, "private", model.InboxItem{ID: "private", ProjectID: private.ID, SourceURL: "https://example.com/private", Remove: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, agent := testAgent(t, f, owner, p.ID, "read")
	for _, tc := range []struct {
		name, token string
		count       int
	}{{"owner", owner, 3}, {"agent", agent, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			var entries []struct {
				Namespace  string           `json:"namespace"`
				InboxCount int              `json:"inbox_count"`
				Inbox      []map[string]any `json:"inbox"`
			}
			if c := f.request("GET", "/api/projects", tc.token, nil, &entries); c != 200 || len(entries) != tc.count {
				t.Fatal(c, entries)
			}
			for _, entry := range entries {
				switch entry.Namespace {
				case p.Namespace:
					if entry.InboxCount != 2 || len(entry.Inbox) != 2 {
						t.Fatal(entry)
					}
					for i, item := range entry.Inbox {
						wantURL, wantFields := "https://example.com/a", 1
						if i == 1 {
							wantURL, wantFields = "https://example.com/b", 2
							if item["remove"] != true {
								t.Fatal(item)
							}
						}
						if len(item) != wantFields || item["source_url"] != wantURL || item["status"] != nil {
							t.Fatal(item)
						}
					}
				case empty.Namespace, "owner/general":
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
