package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/runner/claudemanaged"
	"github.com/mash/kioku/internal/runner/claudemanaged/fake"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/internal/wiki"
)

// These fixtures exercise the pre-cutover HTTP lifecycle.
type legacyManagedClient struct{ claudemanaged.Client }

func managedFixture(t *testing.T) (*fixture, string, fixtureProject, *fake.Server) {
	t.Helper()
	f, token, p := orgProject(t)
	ma := fake.New()
	t.Cleanup(ma.Close)
	f.auto = ma
	f.runnerConfig = runners.Config{AnthropicAPIKey: "fake-key", MAAgentID: "agent_test", MAEnvironmentID: "env_test", MABaseURL: ma.URL(), ManagedAgents: legacyManagedClient{claudemanaged.NewAPI(ma.URL(), "fake-key", ma.HTTP.Client())}, BaseURL: f.s.Config.BaseURL}
	f.setRunners()
	return f, token, p, ma
}
func managedJob(t *testing.T, f *fixture, token, pid string, ensureItem bool) fixtureJob {
	t.Helper()
	if ensureItem {
		waiting := false
		f.stateStore.Read(context.Background(), func(st *store.Queries) error {
			for _, item := range fixtureInboxList(t, st) {
				if item.ProjectID == pid && item.RunID == "" {
					waiting = true
				}
			}
			return nil
		})
		if !waiting {
			if c := f.request("POST", "/api/projects/"+pid+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/fixture"}, nil); c != 202 {
				t.Fatal(c)
			}
		}
	}
	var job fixtureJob
	if c := f.request("POST", "/api/projects/"+pid+"/generate", token, nil, &job); c != 202 {
		t.Fatal(c)
	}
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		job = fixtureJobsGet(t, st, job.ID)
		job.Status = "queued"
		fixtureJobsPut(t, st, job.ID, job)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return job
}
func storedJob(t *testing.T, f *fixture, id string) fixtureJob {
	t.Helper()
	var j fixtureJob
	if err := f.stateStore.Read(context.Background(), func(st *store.Queries) error { j = fixtureJobsGet(t, st, id); return nil }); err != nil {
		t.Fatal(err)
	}
	if f.auto != nil && j.Run != nil && j.Run.Ref.ID != "" {
		if ids := f.auto.Session(j.Run.Ref.ID).VaultIDs; len(ids) > 0 {
			j.Run.Ref.Resources["vault_id"] = ids[0]
		}
	}
	return j
}
func startManaged(t *testing.T, f *fixture, token, pid string, ensureItem bool) fixtureJob {
	t.Helper()
	j := managedJob(t, f, token, pid, ensureItem)
	if err := f.s.startAgentJob(context.Background(), fixtureRow(j)); err != nil && !errors.Is(err, runner.ErrTemporary) {
		t.Fatal(err)
	}
	j = storedJob(t, f, j.ID)
	if j.Status == "failed" {
		t.Fatal(j.Error)
	}
	return f.completeFixtureViews(j)
}
func readInput(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = string(b)
	}
	return out
}
func sendOutput(t *testing.T, f *fixture, ma *fake.Server, j fixtureJob, title string) {
	t.Helper()
	raw := ma.Credential(j.Run.Ref.Resources["vault_id"])
	pages := map[string]string{"wiki/index.md": "# " + title + "\n\n[Notes](notes.md)", "wiki/notes.md": "# Notes\n"}
	previous, items, err := f.s.outputContext(t.Context(), fixtureRow(j))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range previous {
		if strings.HasPrefix(name, "wiki/source-") {
			pages[name] = body
		}
	}
	for _, item := range items {
		name := "wiki/source-" + strings.ToLower(item.ID) + ".md"
		// IDs may contain underscores; filenames use the output alphabet.
		name = strings.ReplaceAll(name, "_", "-")
		for old, body := range pages {
			if u, err := wiki.SourceURL(body); err == nil && u == item.SourceURL {
				name = old
			}
		}
		if item.Remove {
			delete(pages, name)
			continue
		}
		if !item.Remove {
			body, err := f.s.Blobs.Get(t.Context(), item.BodyKey())
			if err != nil {
				t.Fatal(err)
			}
			pages[name] = "---\nsource_url: " + item.SourceURL + "\n---\n" + string(body)
		}
	}
	for name := range pages {
		if strings.HasPrefix(name, "wiki/source-") {
			pages["wiki/notes.md"] += "[Source](" + strings.TrimPrefix(name, "wiki/") + ")\n"
		}
	}
	pages["wiki/log.md"] = "# Log\n" + j.ID + "\n"
	if c, b := agentRequest(t, f, j, raw, "PUT", "output", outputArchive(t, pages)); c != 200 {
		t.Fatalf("output %d: %s", c, b)
	}
}
func reconcileManaged(t *testing.T, f *fixture, j fixtureJob) fixtureJob {
	t.Helper()
	if err := f.s.reconcileAgentJob(context.Background(), fixtureRow(j)); err != nil && !errors.Is(err, runner.ErrTemporary) {
		t.Fatal(err)
	}
	current := storedJob(t, f, j.ID)
	if f.auto != nil && (current.Run == nil || current.Run.CleanupComplete) {
		return f.completeFixtureViews(current)
	}
	return current
}
func assertAgentCleanup(t *testing.T, f *fixture, ma *fake.Server, j fixtureJob) {
	t.Helper()
	if !ma.Session(j.Run.Ref.ID).Archived || !ma.VaultArchived(j.Run.Ref.Resources["vault_id"]) {
		t.Error("remote resources remain active")
	}
	if c, _ := agentRequest(t, f, j, ma.Credential(j.Run.Ref.Resources["vault_id"]), "GET", "input", nil); c != 403 && c != 409 {
		t.Error("token still active", c)
	}
	keys, err := f.s.Blobs.List(context.Background(), fixtureJobPrefix(j))
	if err != nil || len(keys) != 0 {
		t.Error(keys, err)
	}
}
func TestManagedStopReasonsAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason                string
		output, timeout, success, interrupt bool
	}{
		{name: "no output", status: "idle", reason: "end_turn"},
		{name: "budget before output", status: "idle", reason: "budget_reached"},
		{name: "budget after output", status: "idle", reason: "budget_reached", output: true, success: true},
		{name: "exhausted", status: "idle", reason: "retries_exhausted", output: true, success: true},
		{name: "requires action", status: "idle", reason: "requires_action"},
		{name: "terminated output", status: "terminated", output: true, success: true},
		{name: "terminated empty", status: "terminated"},
		{name: "timeout", status: "running", timeout: true, interrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			putFile(t, f, token, p.ID, "x.md", "evidence")
			j := startManaged(t, f, token, p.ID, false)
			if tc.output {
				sendOutput(t, f, ma, j, "Result")
			}
			ma.SetStatus(j.Run.Ref.ID, tc.status, tc.reason, 123)
			if tc.timeout {
				_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
					x := fixtureJobsGet(t, st, j.ID)
					x.Run.Deadline = time.Now().Add(-time.Second)
					fixtureJobsPut(t, st, j.ID, x)
					return nil
				})
			}
			if tc.timeout {
				if err := f.s.reconcileAgentJob(context.Background(), fixtureRow(j)); !errors.Is(err, runner.ErrTemporary) {
					t.Fatal(err)
				}
				pending := storedJob(t, f, j.ID)
				if pending.Status != "running" || pending.Run.AbortReason != "agent job timed out" {
					t.Fatal(pending)
				}
			}
			got := reconcileManaged(t, f, j)
			want := "failed"
			if tc.success {
				want = "completed"
			}
			if got.Status != want {
				t.Fatal(got)
			}
			if tc.interrupt && !ma.Session(j.Run.Ref.ID).Interrupted {
				t.Error("not interrupted")
			}
			assertAgentCleanup(t, f, ma, got)
			_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
				if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 123 {
					t.Error("usage not recorded")
				}
				if !tc.success && fixtureProjectsGet(t, st, p.ID).Revision != "" {
					t.Error("failed job published")
				}
				return nil
			})
		})
	}
}
func TestManagedRegenerationAndUnknownUsage(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	j = reconcileManaged(t, f, j)
	oldAttempt := j.Run.Attempt.ID
	if costOf(j.Run.CostCents) != 123 {
		t.Fatal("lost first cost", costOf(j.Run.CostCents))
	}
	j = startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Retry")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" || costOf(j.Run.CostCents) != 123 || j.Run.Attempt.ID == oldAttempt {
		t.Error("new generation did not finish as a new attempt", j.Status, costOf(j.Run.CostCents))
	}
	j = startManaged(t, f, token, p.ID, true)
	ma.SetError("GET", "/v1/sessions/"+j.Run.Ref.ID, 503)
	if got := reconcileManaged(t, f, j); got.Status != "running" {
		t.Fatal(got)
	}
	_ = f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		x := fixtureJobsGet(t, st, j.ID)
		x.Run.Deadline = time.Now().Add(-11 * time.Minute)
		fixtureJobsPut(t, st, j.ID, x)
		return nil
	})
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" {
		t.Fatal(j)
	}
	_ = f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if costOf(fixtureJobsGet(t, st, j.ID).Run.CostCents) != 0 {
			t.Error("unknown usage recorded a cost")
		}
		return nil
	})
}
func costOf(c *int64) int64 {
	if c == nil {
		return 0
	}
	return *c
}

func deferredPublication(t *testing.T, f *fixture, j fixtureJob) fixtureJob {
	t.Helper()
	if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(j)); err == nil {
		t.Fatal("expected publication failure")
	}
	current := storedJob(t, f, j.ID)
	if current.Status != "running" || current.Run.Phase != model.PhaseFinalizing || current.Run.CleanupComplete {
		t.Fatal("temporary failure lost resumable job", current)
	}
	return current
}
