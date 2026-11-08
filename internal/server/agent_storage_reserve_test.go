package server

import (
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestManagedAcceptedLargeOutputCanPublish(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	f.s.Config.MaxWikiBytes = 1 << 20
	f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: "Source body", SourceURL: "https://example.com/missing"}}, nil)
	j := startManaged(t, f, token, p.ID, false)
	pages := map[string]string{"wiki/index.md": "# Large wiki", "wiki/source-original.md": "---\nsource_url: https://example.com/missing\n---\nSource body"}
	pages["wiki/large.md"] = strings.Repeat("a", (1<<20)+100)
	code, body := agentRequest(t, f, j, ma.Credential(j.Run.Ref.Resources["vault_id"]), "PUT", "output", outputArchive(t, pages))
	if code != 200 {
		t.Fatalf("upload: %d %s", code, body)
	}
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" {
		t.Fatalf("accepted output could not publish: %s: %s", j.Status, j.Error)
	}
	if code := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: "Source body", SourceURL: "https://example.com/new"}}, nil); code != 413 {
		t.Fatal("quota", code)
	}
	if code := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/missing"}, nil); code != 202 {
		t.Fatal("removal blocked", code)
	}
}
