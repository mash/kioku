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
	valid := SourceContentForTest("https://example.com/evidence", "Evidence")
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"frontmatter", valid, true},
		{"missing", "Evidence", false},
		{"missing source_url", strings.ReplaceAll(valid, "source_url: https://example.com/evidence\n", ""), false},
		{"missing updated_at", strings.ReplaceAll(valid, "updated_at: "+testUpdatedAt+"\n", ""), false},
		{"empty updated_at", strings.ReplaceAll(valid, testUpdatedAt, `""`), false},
		{"invalid updated_at", strings.ReplaceAll(valid, testUpdatedAt, "yesterday"), false},
		{"offset required", strings.ReplaceAll(valid, testUpdatedAt, "2026-11-07T18:00:00"), false},
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
				json.NewEncoder(w).Encode(struct{}{})
			}))
			defer server.Close()
			var out bytes.Buffer
			a := &App{Dir: t.TempDir(), Config: Config{Server: server.URL, Namespace: "acme/platform"}, Client: server.Client(), Out: &out, Err: &out, ctx: context.Background()}
			file := filepath.Join(t.TempDir(), "notes.md")
			if err := os.WriteFile(file, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			err := a.put([]string{file})
			if tc.valid && out.String() != "accepted notes.md\n" {
				t.Fatal("acceptance output", out.String())
			}
			if !tc.valid {
				_, want := source.FileWithProvenance("notes.md", tc.body)
				if err == nil || want == nil || !strings.HasSuffix(err.Error(), want.Error()) {
					t.Fatalf("CLI error: %v; want %v", err, want)
				}
			}

			if (err == nil) != tc.valid || (!tc.valid && writes != 0) || (tc.valid && writes != 1) {
				t.Fatal("result", err, "writes", writes)
			}
		})
	}
}
