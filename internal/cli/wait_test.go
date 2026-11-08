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

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func TestWritesReturnWithoutPollingAndOnlyGenerationWaits(t *testing.T) {
	polls, snapshots, writes := 0, 0, 0
	jobKind := "generate"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/snapshot"):
			if r.URL.Query().Get("manifest") == "1" {
				snapshots++
				json.NewEncoder(w).Encode(model.SyncManifest{Revision: "revision", Files: map[string]model.SyncFile{"wiki/index.md": {Hash: auth.Hash("# Completed"), Bytes: 11}}})
			} else {
				json.NewEncoder(w).Encode(model.SnapshotFile{Body: "# Completed", Hash: auth.Hash("# Completed")})
			}
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/jobs/"):
			polls++
			json.NewEncoder(w).Encode(model.Job{ID: "job", Kind: jobKind, Status: "completed", Revision: "revision"})
		default:
			writes++
			jobKind = "generate"
			if strings.HasSuffix(r.URL.Path, "/put") || r.Method == "DELETE" {
				jobKind = "put"
			}
			w.WriteHeader(202)
			json.NewEncoder(w).Encode(model.Job{ID: "job", Kind: jobKind, Status: "queued"})
		}
	}))
	defer server.Close()
	var out, stderr bytes.Buffer
	a := &App{Dir: t.TempDir(), Config: Config{Server: server.URL, Namespace: "acme/platform"}, Client: server.Client(), Out: &out, Err: &stderr, ctx: context.Background()}
	file := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(file, []byte(SourceContentForTest("https://example.com/notes", "Evidence")), 0600); err != nil {
		t.Fatal(err)
	}
	operations := []func() error{
		func() error { return a.put([]string{file}) },
		func() error { return a.deleteSources([]string{"https://example.com/source"}) },
		func() error { return a.job([]string{"retry", "job"}) },
		func() error { return a.generate(nil) },
	}
	for _, operation := range operations {
		out.Reset()
		if err := operation(); err != nil {
			t.Fatal(err)
		}
		var job model.Job
		if err := json.Unmarshal(out.Bytes(), &job); err != nil || job.ID != "job" || job.Status != "queued" {
			t.Fatal("write did not return accepted job JSON", out.String(), err)
		}
	}
	if polls != 0 || snapshots != 0 || writes != len(operations) {
		t.Fatal("default writes polled or pulled", polls, snapshots, writes)
	}
	if err := a.put([]string{"--wait", file}); err == nil {
		t.Fatal(err)
	}
	if polls != 0 || snapshots != 0 {
		t.Fatal("put --wait pulled wiki", polls, snapshots)
	}
	if err := a.generate([]string{"--wait"}); err != nil {
		t.Fatal(err)
	}
	if polls != 1 || snapshots != 1 {
		t.Fatal("generate --wait did not pull", polls, snapshots)
	}
	body, err := os.ReadFile(filepath.Join(a.localDir(), "wiki", "index.md"))
	if err != nil || string(body) != "# Completed" {
		t.Fatal("completed snapshot not pulled", err)
	}
	if err := a.job([]string{"wait", "job"}); err != nil {
		t.Fatal(err)
	}
	if polls != 2 || snapshots != 2 {
		t.Fatal("job wait did not pull completed job", polls, snapshots)
	}
	if err := a.job([]string{"retry", "--wait", "job"}); err != nil {
		t.Fatal(err)
	}
	if polls != 3 || snapshots != 3 {
		t.Fatal("retry --wait did not wait and pull", polls, snapshots)
	}
}

func TestWaitingForUnchangedEmptyProjectDoesNotPull(t *testing.T) {
	var out bytes.Buffer
	a := &App{Dir: t.TempDir(), Config: Config{Namespace: "acme/platform"}, Out: &out, ctx: context.Background()}
	if err := a.wait(model.Job{ID: "job", Kind: "generate", Status: "completed", Unchanged: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No waiting inbox items") {
		t.Fatal("missing unchanged result", out.String())
	}
}
