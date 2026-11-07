package server

import (
	"bytes"
	"testing"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

func TestOutputEndpointReportsEachSourceRule(t *testing.T) {
	page := "---\nsource_url: https://example.com/a\n---\nBody"
	for _, rule := range []string{"a", "b", "c", "d", "e"} {
		t.Run(rule, func(t *testing.T) {
			f, raw, j, _, _ := jobTokenFixture(t)
			setManagedPhase(t, f, j, model.PhasePreparing, "running")
			pages := map[string]string{"wiki/index.md": "# Wiki"}
			switch rule {
			case "a":
				pages["wiki/source-a.md"] = "Missing frontmatter"
			case "b":
				pages["wiki/source-a.md"] = page
				pages["wiki/source-b.md"] = page
			case "c":
				pages["wiki/source-a.md"] = page
			}
			if rule == "c" || rule == "e" {
				f.db.Update(func(st *model.State) error {
					job := st.Jobs[j.ID]
					job.Run.Attempt.ID = "attempt"
					job.Run.InboxIDs = []string{"item"}
					st.Jobs[j.ID] = job
					status := "removed"
					if rule == "e" {
						status = "fetched"
					}
					st.Inbox["item"] = model.InboxItem{ID: "item", ProjectID: j.ProjectID, RunID: "attempt", Status: status, SourceURL: "https://example.com/a"}
					return nil
				})
			}
			if rule == "d" {
				key := blob.Key("projects/" + j.ProjectID + "/wiki/source-a.md")
				if err := f.s.Blobs.Put(t.Context(), key, []byte(page)); err != nil {
					t.Fatal(err)
				}
				f.db.Update(func(st *model.State) error {
					p := st.Projects[j.ProjectID]
					p.Revision = "previous"
					st.Projects[p.ID] = p
					st.Revisions[p.Revision] = model.Revision{ID: p.Revision, ProjectID: p.ID, Files: map[string]blob.Key{"wiki/source-a.md": key}}
					return nil
				})
			}
			code, body := agentRequest(t, f, j, raw, "PUT", "output", outputArchive(t, pages))
			if code != 400 || !bytes.Contains(body, []byte("("+rule+")")) || !bytes.Contains(body, []byte(`"problems"`)) {
				t.Fatal(code, string(body))
			}
			if got := storedJob(t, f, j.ID); got.Run.OutputHash != "" {
				t.Fatal("rejected output selected")
			}
		})
	}
}
