package server

import (
	"strings"
	"testing"

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
		{"missing frontmatter", "Evidence", "frontmatter"},
		{"empty frontmatter before provenance", "---\n" + valid, "frontmatter"},
		{"missing source_url", strings.ReplaceAll(valid, "source_url: https://example.com/a\n", ""), "source_url"},
		{"missing updated_at", strings.ReplaceAll(valid, "updated_at: "+testUpdatedAt+"\n", ""), "updated_at"},
		{"empty updated_at", strings.ReplaceAll(valid, testUpdatedAt, `""`), "updated_at"},
		{"invalid updated_at", strings.ReplaceAll(valid, testUpdatedAt, "yesterday"), "updated_at"},
		{"offset required", strings.ReplaceAll(valid, testUpdatedAt, "2026-11-07T18:00:00"), "updated_at"},
		{"offset hour overflow", strings.ReplaceAll(valid, testUpdatedAt, "2026-11-07T18:00:00+24:00"), "updated_at"},
		{"offset minute overflow", strings.ReplaceAll(valid, testUpdatedAt, "2026-11-07T18:00:00+09:60"), "updated_at"},
		{"single digit hour", strings.ReplaceAll(valid, testUpdatedAt, "2026-11-07T8:00:00+09:00"), "updated_at"},
		{"comma fraction", strings.ReplaceAll(valid, testUpdatedAt, "2026-11-07T18:00:00,123+09:00"), "updated_at"},
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
	for _, tc := range []struct{ url, errorKey string }{
		{"https://example.com/original", "source_url argument is only for remove"},
		{"https://example.com/override", "source_url argument is only for remove"},
		{"", "invalid arguments"},
	} {
		for _, req := range []any{
			map[string]any{"file": map[string]any{"name": "a.md", "content": content, "source_url": tc.url}},
			map[string]any{"file": map[string]any{"name": "a.md", "content": content}, "source_url": tc.url},
		} {
			if c := f.request("POST", "/api/projects/"+p.ID+"/put", human, req, nil); c != 400 {
				t.Fatalf("accepted external provenance: %d, %+v", c, req)
			}
		}
		args := map[string]any{"project": p.Namespace, "name": "a.md", "content": content, "source_url": tc.url}
		if msg := connectorTool(t, f, access, "source_put", args, nil); !strings.Contains(msg, tc.errorKey) {
			t.Fatal("MCP accepted external provenance:", msg)
		}
	}
}

func TestInboxStoresSourceURLAndVerbatimBody(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	access := connectorAccessToken(t, f, human, p.ID)
	content := strings.Replace(testSourceContent("https://example.com/a", "Evidence"), "---\nEvidence", "remove: true\nsubmitter: forged\n---\nEvidence", 1)
	for _, mcp := range []bool{false, true} {
		var result map[string]any
		if mcp {
			args := map[string]any{"project": p.Namespace, "name": "a.md", "content": content}
			if msg := connectorTool(t, f, access, "source_put", args, &result); msg != "" {
				t.Fatal(msg)
			}
		} else if c := f.request("POST", "/api/projects/"+p.ID+"/put", human, model.PutRequest{File: &model.FileInput{Name: "a.md", Content: content}}, &result); c != 202 {
			t.Fatal(c)
		}
		if result == nil || len(result) != 0 {
			t.Fatal("non-empty put response", result)
		}
		item := waitingInboxItem(t, f, p.ID, "https://example.com/a")
		if item.SourceURL != "https://example.com/a" || item.Remove || item.Submitter == "forged" {
			t.Fatalf("stored item: %+v", item)
		}
		body, err := f.s.Blobs.Get(t.Context(), item.BodyKey())
		if err != nil || string(body) != content {
			t.Fatalf("body: %q, %v", body, err)
		}
	}
}
