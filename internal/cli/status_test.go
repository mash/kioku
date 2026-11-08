package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestStatusReadsInboxFromProjectListing(t *testing.T) {
	for _, items := range [][]model.InboxEntry{{}, {{SourceURL: "https://example.com/a", Status: "fetched"}, {SourceURL: "https://example.com/b", Status: "removed"}}} {
		t.Run(fmt.Sprint(len(items)), func(t *testing.T) {
			requests := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != "GET" || r.URL.Path != "/api/projects" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				json.NewEncoder(w).Encode([]model.ProjectEntry{
					{Project: model.Project{ID: "other"}, InboxCount: 1, Inbox: []model.InboxEntry{{SourceURL: "https://example.com/other", Status: "fetched"}}},
					{Project: model.Project{Namespace: "org/selected", Revision: "current"}, InboxCount: len(items), Inbox: items},
				})
			}))
			defer ts.Close()
			var out bytes.Buffer
			a := &App{Dir: t.TempDir(), Config: Config{Server: ts.URL, Namespace: "org/selected"}, Client: ts.Client(), Out: &out, Err: io.Discard, ctx: context.Background()}
			if err := os.MkdirAll(a.metadataDir(), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(a.metadataDir(), "manifest.json"), []byte("invalid old manifest"), 0400); err != nil {
				t.Fatal(err)
			}
			if err := a.status(); err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"local_revision", "local_sync_incomplete"} {
				if _, ok := fields[key]; ok {
					t.Fatalf("obsolete status field %s", key)
				}
			}
			var result struct {
				InboxCount int                `json:"inbox_count"`
				Inbox      []model.InboxEntry `json:"inbox"`
				Revision   string             `json:"remote_revision"`
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if requests != 1 || result.InboxCount != len(items) || !reflect.DeepEqual(result.Inbox, items) || result.Revision != "current" {
				t.Fatal(requests, out.String())
			}
		})
	}
}
