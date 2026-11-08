package server

import (
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
)

// The fake supplies model output. This verifies the archive/publication contract,
// not whether a real model follows the prompt's filename rules.
func TestSourcePagesRoundTripPreservesNamesAndSlugCollision(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	urls := []string{"https://example.com/a", "https://example.com/b"}
	putText := func(url, body string) {
		t.Helper()
		if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent(url, body)}}, nil); c != 202 {
			t.Fatal(c)
		}
	}
	putText(urls[0], "# Title\nFirst")
	putText(urls[1], "# Title\nSecond")
	pages := map[string]string{
		"wiki/index.md":          "# Wiki\n[Analysis](analysis.md)",
		"wiki/analysis.md":       "[One](source-title.md) and [Two](source-title-2.md)",
		"wiki/source-title.md":   "---\nsource_url: " + urls[0] + "\n---\n# Title\nFirst",
		"wiki/source-title-2.md": "---\nsource_url: " + urls[1] + "\n---\n# Title\nSecond",
		"wiki/log.md":            "# Log\nAdded two sources",
	}
	publish := func(j model.Job) {
		t.Helper()
		raw := ma.Credential(j.Run.Ref.Resources["vault_id"])
		if c, b := agentRequest(t, f, j, raw, "PUT", "output", outputArchive(t, pages)); c != 200 {
			t.Fatal(c, string(b))
		}
		ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
		if j = reconcileManaged(t, f, j); j.Status != "completed" {
			t.Fatal(j)
		}
	}
	publish(startManaged(t, f, token, p.ID, false))
	putText(urls[0], "# Title\nFirst\nUpdated from same URL")
	j := startManaged(t, f, token, p.ID, false)
	c, b := agentRequest(t, f, j, ma.Credential(j.Run.Ref.Resources["vault_id"]), "GET", "input", nil)
	if c != 200 {
		t.Fatal(c, string(b))
	}
	input := readInput(t, b)
	for name, body := range pages {
		if input[name] != body {
			t.Fatalf("previous page changed: %s", name)
		}
	}
	pages["wiki/source-title.md"] += "\nUpdated from same URL"
	pages["wiki/log.md"] = "# Log\nUpdated one source"
	publish(j)
	if err := f.db.View(func(st model.State) error {
		r := st.Revisions[st.Projects[p.ID].Revision]
		count := 0
		for name, key := range r.Files {
			if !strings.HasPrefix(name, "wiki/source-") {
				continue
			}
			count++
			body, err := f.s.Blobs.Get(t.Context(), key)
			if err != nil || string(body) != pages[name] {
				t.Fatalf("published source %s: %q, %v", name, body, err)
			}
		}
		if count != 2 || len(st.Inbox) != 0 {
			t.Fatal(count, st.Inbox)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
