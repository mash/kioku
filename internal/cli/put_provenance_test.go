package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestPutSourceURL(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		flags      []string
		valid      bool
	}{
		{"explicit", "Evidence", []string{"--source-url", "https://example.com/evidence"}, true},
		{"frontmatter", "---\nsource_url: https://example.com/evidence\n---\nEvidence", nil, true},
		{"missing", "Evidence", nil, false},
		{"invalid", "Evidence", []string{"--source-url", "not-a-url"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writes++
				var req model.PutRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.File == nil || req.File.SourceURL != "https://example.com/evidence" || req.File.Content != tc.body {
					t.Errorf("input: %+v", req)
				}
				w.WriteHeader(202)
				json.NewEncoder(w).Encode(model.Job{ID: "job", Status: "completed"})
			}))
			defer server.Close()
			var out bytes.Buffer
			a := &App{Dir: t.TempDir(), Config: Config{Server: server.URL, Project: "project", Namespace: "acme/platform"}, Client: server.Client(), Out: &out, Err: &out, ctx: context.Background()}
			file := filepath.Join(t.TempDir(), "notes.md")
			if err := os.WriteFile(file, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			err := a.put(append(tc.flags, file))
			if (err == nil) != tc.valid || (!tc.valid && writes != 0) || (tc.valid && writes != 1) {
				t.Fatal("result", err, "writes", writes)
			}
		})
	}
}
