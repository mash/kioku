package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

func TestPutProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		flags      []string
		valid      bool
	}{
		{"removed flag", "Evidence", []string{"--source-url", "https://example.com/evidence"}, false},
		{"frontmatter", "---\nsource_url: https://example.com/evidence\nupdated_at: 2026-11-07T18:00:00+09:00\n---\nEvidence", nil, true},
		{"missing", "Evidence", nil, false},
		{"missing source_url", "---\nupdated_at: 2026-11-07T18:00:00+09:00\n---\nEvidence", nil, false},
		{"missing updated_at", "---\nsource_url: https://example.com/evidence\n---\nEvidence", nil, false},
		{"empty updated_at", "---\nsource_url: https://example.com/evidence\nupdated_at: \"\"\n---\nEvidence", nil, false},
		{"invalid updated_at", "---\nsource_url: https://example.com/evidence\nupdated_at: yesterday\n---\nEvidence", nil, false},
		{"offset required", "---\nsource_url: https://example.com/evidence\nupdated_at: 2026-11-07T18:00:00\n---\nEvidence", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writes++
				var req model.PutRequest
				decoder := json.NewDecoder(r.Body)
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&req); err != nil {
					t.Error(err)
				}
				if req.File == nil || req.SourceURL != "" || req.File.Content != tc.body {
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
			if !tc.valid && len(tc.flags) == 0 {
				_, want := source.FileWithProvenance("notes.md", tc.body)
				if err == nil || want == nil || !strings.HasSuffix(err.Error(), want.Error()) {
					t.Fatalf("CLI error: %v; want %v", err, want)
				}
			}
			if tc.name == "removed flag" && (err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -source-url")) {
				t.Fatalf("removed flag error: %v", err)
			}
			if (err == nil) != tc.valid || (!tc.valid && writes != 0) || (tc.valid && writes != 1) {
				t.Fatal("result", err, "writes", writes)
			}
		})
	}
}
