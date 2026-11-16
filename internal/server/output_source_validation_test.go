package server

import (
	"bytes"
	"context"
	"testing"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func TestOutputEndpointReportsEachSourceRule(t *testing.T) {
	t.Parallel()
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
				f.stateStore.Write(context.Background(), func(st *store.Queries) error {
					job := fixtureJobsGet(t, st, j.ID)
					job.Run.InboxIDs = []string{"item"}
					fixtureJobsPut(t, st, j.ID, job)
					fixtureInboxPut(t, st, "item", model.InboxItem{ID: "item", ProjectID: j.ProjectID, RunID: j.ID, Remove: rule == "c", SourceURL: "https://example.com/a"})
					return nil
				})
			}
			if rule == "d" {
				key := model.WikiContentKey(j.ProjectID, model.HashContent([]byte(page)))
				if err := f.s.Blobs.Put(t.Context(), key, []byte(page)); err != nil {
					t.Fatal(err)
				}
				f.stateStore.Write(context.Background(), func(st *store.Queries) error {
					p := fixtureProjectsGet(t, st, j.ProjectID)
					p.Revision = "previous"
					fixtureProjectsPut(t, st, p.ID, p)
					fixtureRevisionsPut(t, st, p.Revision, fixtureRevision{ID: p.Revision, ProjectID: p.ID, Files: map[string]blob.Key{"wiki/source-a.md": key}, FileBytes: map[string]int64{"wiki/source-a.md": int64(len(page))}})
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
