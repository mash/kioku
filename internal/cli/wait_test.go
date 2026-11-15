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
			job := model.Job{ID: "job", Kind: jobKind, Status: "completed", WikiStatus: "completed", Revision: "revision"}
			if polls == 1 {
				job.Status = "running"
				job.Views = []model.ViewJobResult{{Slug: "project-map", Status: "running"}}
			}
			json.NewEncoder(w).Encode(job)
		default:
			writes++
			jobKind = "generate"
			if strings.HasSuffix(r.URL.Path, "/put") {
				w.WriteHeader(202)
				json.NewEncoder(w).Encode(struct{}{})
				return
			}
			w.WriteHeader(202)
			json.NewEncoder(w).Encode(model.Job{ID: "job", Kind: jobKind, Status: "queued"})
		}
	}))
	defer server.Close()
	var out, stderr bytes.Buffer
	a := &App{Dir: t.TempDir(), AuthDir: filepath.Join(t.TempDir(), "kioku", "auth"), Config: Config{Server: server.URL, Namespace: "acme/platform"}, Client: server.Client(), Out: &out, Err: &stderr, ctx: context.Background()}
	file := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(file, []byte(SourceContentForTest("https://example.com/notes", "Evidence")), 0600); err != nil {
		t.Fatal(err)
	}
	operations := []func() error{
		func() error { return a.put([]string{file}) },
		func() error {
			return a.deleteSources([]string{"https://example.com/source", "https://example.com/another"})
		},
		func() error { return a.generate(nil) },
	}
	for i, operation := range operations {
		out.Reset()
		if err := operation(); err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			want := []string{"accepted note.md\n", "removed https://example.com/source\nremoved https://example.com/another\n"}[i]
			if out.String() != want {
				t.Fatal("acceptance output", out.String())
			}
			continue
		}
		var job model.Job
		if err := json.Unmarshal(out.Bytes(), &job); err != nil || job.ID != "job" || job.Status != "queued" {
			t.Fatal("write did not return accepted job JSON", out.String(), err)
		}
	}
	if polls != 0 || snapshots != 0 || writes != len(operations)+1 {
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
	if polls != 2 || snapshots != 1 {
		t.Fatal("generate --wait did not pull", polls, snapshots)
	}
	body, err := os.ReadFile(filepath.Join(a.localDir(), "wiki", "index.md"))
	if err != nil || string(body) != "# Completed" {
		t.Fatal("completed snapshot not pulled", err)
	}
	if err := a.job([]string{"wait", "job"}); err != nil {
		t.Fatal(err)
	}
	if polls != 3 || snapshots != 2 {
		t.Fatal("job wait did not pull completed job", polls, snapshots)
	}
	if err := a.job([]string{"retry", "--wait", "job"}); err == nil {
		t.Fatal("removed retry command was accepted")
	}
	if polls != 3 || snapshots != 2 || writes != len(operations)+2 {
		t.Fatal("removed retry command made a request", polls, snapshots, writes)
	}
	if err := a.wait(model.Job{ID: "job", Kind: "generate", Status: "failed", Error: "generation failed"}); err == nil ||
		!strings.Contains(err.Error(), "kioku generate --wait") {
		t.Fatal("missing regeneration advice", err)
	}
}

func TestWaitingForUnchangedEmptyProjectDoesNotPull(t *testing.T) {
	var out bytes.Buffer
	a := &App{Dir: t.TempDir(), AuthDir: filepath.Join(t.TempDir(), "kioku", "auth"), Config: Config{Namespace: "acme/platform"}, Out: &out, ctx: context.Background()}
	if err := a.wait(model.Job{ID: "job", Kind: "generate", Status: "completed", Unchanged: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No waiting inbox items") {
		t.Fatal("missing unchanged result", out.String())
	}
	out.Reset()
	job := model.Job{ID: "job", Kind: "generate", Status: "completed", Unchanged: true, Revision: "partial", WikiStatus: "unavailable", Views: []model.ViewJobResult{{Slug: "project-map", Status: "skipped"}}}
	if err := a.wait(job); err != nil {
		t.Fatal(err)
	}
	var result model.Job
	_, line, _ := strings.Cut(out.String(), "\n")
	if err := json.Unmarshal([]byte(line), &result); err != nil || result.WikiStatus != "unavailable" || len(result.Views) != 1 {
		t.Fatalf("unavailable Wiki wait should return its job: %s", out.String())
	}
}

func TestSessionJobWaitRecordsPull(t *testing.T) {
	sessionTestEnv(t)
	a, _, _, status := syncFixture(t)
	job := model.Job{ID: "job", Kind: "generate", Status: "completed", Revision: "revision"}
	if err := a.wait(job); err != nil {
		t.Fatal(err)
	}
	if s := mustSession(t); s.LastPullResult != sessionPullSuccess || s.Path != a.Dir || s.Namespace != a.Config.Namespace {
		t.Fatal(s)
	}
	*status = 403
	if err := a.wait(job); err == nil {
		t.Fatal("expected pull failure")
	}
	if s := mustSession(t); s.LastPullResult != sessionPullFailure {
		t.Fatal(s)
	}
}
