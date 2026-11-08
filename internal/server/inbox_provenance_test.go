package server

import (
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

func TestInboxProvenanceValidationMatchesMCP(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	access := connectorAccessToken(t, f, human, p.ID)
	valid := testSourceContent("https://example.com/a", "Evidence")
	for _, tc := range []struct{ name, body, key string }{
		{"missing frontmatter", "Evidence", "source_url"},
		{"missing source_url", strings.ReplaceAll(valid, "source_url: https://example.com/a\n", ""), "source_url"},
		{"missing updated_at", strings.ReplaceAll(valid, "updated_at: "+testUpdatedAt+"\n", ""), "updated_at"},
		{"empty updated_at", strings.ReplaceAll(valid, testUpdatedAt, `""`), "updated_at"},
		{"invalid updated_at", strings.ReplaceAll(valid, testUpdatedAt, "yesterday"), "updated_at"},
		{"offset required", strings.ReplaceAll(valid, testUpdatedAt, "2026-11-07T18:00:00"), "updated_at"},
		{"invalid url", strings.ReplaceAll(valid, "https://", "http://"), "source_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, want := source.FileWithProvenance("a.md", tc.body)
			if want == nil || !strings.Contains(want.Error(), tc.key) {
				t.Fatalf("expected %s error, got %v", tc.key, want)
			}
			var response struct {
				Error string `json:"error"`
			}
			if c := f.request("POST", "/api/projects/"+p.ID+"/put", human, model.PutRequest{File: &model.FileInput{Name: "a.md", Content: tc.body}}, &response); c != 400 || response.Error != want.Error() {
				t.Fatalf("HTTP: %d, %+v; want %v", c, response, want)
			}
			args := map[string]any{"project": p.Namespace, "name": "a.md", "content": tc.body}
			if msg := connectorTool(t, f, access, "source_put", args, nil); msg != want.Error() {
				t.Fatalf("MCP: %s; want %v", msg, want)
			}
		})
	}
	if err := f.db.View(func(st model.State) error {
		if len(st.Inbox) != 0 || len(st.Jobs) != 0 {
			t.Fatal("invalid sources changed state", st.Inbox)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInboxRejectsExternalFileProvenance(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	access := connectorAccessToken(t, f, human, p.ID)
	content := testSourceContent("https://example.com/original", "Evidence")
	for _, url := range []string{"https://example.com/original", "https://example.com/override", ""} {
		for _, req := range []any{
			map[string]any{"file": map[string]any{"name": "a.md", "content": content, "source_url": url}},
			map[string]any{"file": map[string]any{"name": "a.md", "content": content}, "source_url": url},
		} {
			if c := f.request("POST", "/api/projects/"+p.ID+"/put", human, req, nil); c != 400 {
				t.Fatalf("accepted external provenance: %d, %+v", c, req)
			}
		}
		args := map[string]any{"project": p.Namespace, "name": "a.md", "content": content, "source_url": url}
		if msg := connectorTool(t, f, access, "source_put", args, nil); !strings.Contains(msg, "source_url argument is only allowed with remove: true") {
			t.Fatal("MCP accepted external provenance:", msg)
		}
	}
}

func TestInboxStoresProvenanceAndVerbatimBody(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	access := connectorAccessToken(t, f, human, p.ID)
	content := strings.Replace(testSourceContent("https://example.com/a", "Evidence"), "---\nEvidence", "status: removed\nsubmitter: forged\n---\nEvidence", 1)
	for _, mcp := range []bool{false, true} {
		var result model.InboxItem
		if mcp {
			args := map[string]any{"project": p.Namespace, "name": "a.md", "content": content}
			if msg := connectorTool(t, f, access, "source_put", args, &result); msg != "" {
				t.Fatal(msg)
			}
		} else if c := f.request("POST", "/api/projects/"+p.ID+"/put", human, model.PutRequest{File: &model.FileInput{Name: "a.md", Content: content}}, &result); c != 202 {
			t.Fatal(c)
		}
		if err := f.db.View(func(st model.State) error {
			item := st.Inbox[result.ID]
			if item.SourceURL != "https://example.com/a" || item.Status != "fetched" || item.Submitter == "forged" || item.UpdatedAt == nil || item.UpdatedAt.Format(time.RFC3339) != testUpdatedAt {
				t.Fatalf("stored item: %+v", item)
			}
			body, err := f.s.Blobs.Get(t.Context(), item.BodyKey())
			if err != nil || string(body) != content {
				t.Fatalf("body: %q, %v", body, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var removed model.InboxItem
	if msg := connectorTool(t, f, access, "source_put", map[string]any{"project": p.Namespace, "remove": true, "source_url": "https://example.com/a"}, &removed); msg != "" {
		t.Fatal(msg)
	}
	if err := f.db.View(func(st model.State) error {
		item := st.Inbox[removed.ID]
		if item.Status != "removed" || item.UpdatedAt != nil {
			t.Fatalf("removal: %+v", item)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
