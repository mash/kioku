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

	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/managedagents/fake"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/wiki"
)

func managedFixture(t *testing.T) (*fixture, string, model.Project, *fake.Server) {
	t.Helper()
	f, token, p := orgProject(t)
	ma := fake.New()
	t.Cleanup(ma.Close)
	f.runnerConfig = runners.Config{AnthropicAPIKey: "fake-key", MAAgentID: "agent_test", MAEnvironmentID: "env_test", MABaseURL: ma.URL(), ManagedAgents: managedagents.New(ma.URL(), "fake-key", ma.HTTP.Client()), BaseURL: f.s.Config.BaseURL}
	f.setRunners()
	return f, token, p, ma
}
func managedJob(t *testing.T, f *fixture, token, pid string, ensureItem bool) model.Job {
	t.Helper()
	if ensureItem {
		waiting := false
		f.db.View(func(st model.State) error {
			for _, item := range st.Inbox {
				if item.ProjectID == pid && item.RunID == "" {
					waiting = true
				}
			}
			return nil
		})
		if !waiting {
			if c := f.request("POST", "/api/projects/"+pid+"/put", token, model.PutRequest{URL: "https://example.com/fixture"}, nil); c != 202 {
				t.Fatal(c)
			}
		}
	}
	var job model.Job
	if c := f.request("POST", "/api/projects/"+pid+"/generate", token, nil, &job); c != 202 {
		t.Fatal(c)
	}
	if err := f.db.Update(func(st *model.State) error {
		job = st.Jobs[job.ID]
		job.Status = "running"
		st.Jobs[job.ID] = job
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return job
}
func storedJob(t *testing.T, f *fixture, id string) model.Job {
	t.Helper()
	var j model.Job
	if err := f.db.View(func(st model.State) error { j = st.Jobs[id]; return nil }); err != nil {
		t.Fatal(err)
	}
	return j
}
func startManaged(t *testing.T, f *fixture, token, pid string, ensureItem bool) model.Job {
	t.Helper()
	j := managedJob(t, f, token, pid, ensureItem)
	if err := f.s.startAgentJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j = storedJob(t, f, j.ID)
	if j.Status == "failed" {
		t.Fatal(j.Error)
	}
	return j
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
func sendOutput(t *testing.T, f *fixture, ma *fake.Server, j model.Job, title string) {
	t.Helper()
	raw := ma.Credential(j.Run.Ref.Resources["vault_id"])
	pages := map[string]string{"wiki/index.md": "# " + title + "\n\n[Notes](notes.md)", "wiki/notes.md": "# Notes\n"}
	previous, items, err := f.s.outputContext(t.Context(), j)
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
		if item.Status == "removed" {
			delete(pages, name)
			continue
		}
		if item.Status == "fetched" {
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
	for _, item := range items {
		if item.Status == "pending" {
			pages["wiki/log.md"] += "Fetch failed: " + item.SourceURL + "; reason: unavailable in fixture; submitter: " + item.Submitter + "\n"
		}
	}
	if c, b := agentRequest(t, f, j, raw, "PUT", "output", outputArchive(t, pages)); c != 200 {
		t.Fatalf("output %d: %s", c, b)
	}
}
func reconcileManaged(t *testing.T, f *fixture, j model.Job) model.Job {
	t.Helper()
	if err := f.s.reconcileAgentJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	return storedJob(t, f, j.ID)
}
func assertAgentCleanup(t *testing.T, f *fixture, ma *fake.Server, j model.Job) {
	t.Helper()
	if !ma.Session(j.Run.Ref.ID).Archived || !ma.VaultArchived(j.Run.Ref.Resources["vault_id"]) {
		t.Error("remote resources remain active")
	}
	if c, _ := agentRequest(t, f, j, ma.Credential(j.Run.Ref.Resources["vault_id"]), "GET", "input", nil); c != 403 {
		t.Error("token still active", c)
	}
	keys, err := f.s.Blobs.List(context.Background(), agentJobPrefix(j))
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
				_ = f.db.Update(func(st *model.State) error {
					x := st.Jobs[j.ID]
					x.Run.Deadline = time.Now().Add(-time.Second)
					st.Jobs[j.ID] = x
					return nil
				})
			}
			if tc.timeout {
				if err := f.s.reconcileAgentJob(context.Background(), j); !errors.Is(err, runner.ErrTemporary) {
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
			_ = f.db.View(func(st model.State) error {
				if costOf(st.Jobs[j.ID].Run.CostCents) != 123 {
					t.Error("usage not recorded")
				}
				if !tc.success && st.Projects[p.ID].Revision != "" {
					t.Error("failed job published")
				}
				return nil
			})
		})
	}
}
func TestManagedRetryAndUnknownUsage(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "x.md", "evidence")
	j := startManaged(t, f, token, p.ID, false)
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	j = reconcileManaged(t, f, j)
	oldAttempt := j.Run.Attempt.ID
	if costOf(j.Run.CostCents) != 123 {
		t.Fatal("lost first cost", costOf(j.Run.CostCents))
	}
	var retried model.Job
	if c := f.request("POST", "/api/projects/"+p.ID+"/jobs/"+j.ID+"/retry", token, nil, &retried); c != 202 {
		t.Fatal(c)
	}
	if retried.Run != nil {
		t.Fatal("retry retained run")
	}
	_ = f.db.Update(func(st *model.State) error {
		retried = st.Jobs[j.ID]
		retried.Status = "running"
		st.Jobs[j.ID] = retried
		return nil
	})
	if err := f.s.startAgentJob(context.Background(), retried); err != nil {
		t.Fatal(err)
	}
	j = storedJob(t, f, j.ID)
	sendOutput(t, f, ma, j, "Retry")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 123)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" || costOf(j.Run.CostCents) != 123 || j.Run.Attempt.ID == oldAttempt {
		t.Error("retry did not finish as a new attempt", j.Status, costOf(j.Run.CostCents))
	}
	j = startManaged(t, f, token, p.ID, true)
	ma.SetError("GET", "/v1/sessions/"+j.Run.Ref.ID, 503)
	if got := reconcileManaged(t, f, j); got.Status != "running" {
		t.Fatal(got)
	}
	_ = f.db.Update(func(st *model.State) error {
		x := st.Jobs[j.ID]
		x.Run.Deadline = time.Now().Add(-11 * time.Minute)
		st.Jobs[j.ID] = x
		return nil
	})
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" {
		t.Fatal(j)
	}
	_ = f.db.View(func(st model.State) error {
		if costOf(st.Jobs[j.ID].Run.CostCents) != 0 {
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
