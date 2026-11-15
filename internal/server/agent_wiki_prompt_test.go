package server

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/prompts"
)

func wikiInputArchive(t *testing.T, f *fixture, job model.Job) []byte {
	t.Helper()
	data, err := f.s.Blobs.Get(t.Context(), agentJobKey(job, "input.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWikiPromptGeneration(t *testing.T) {
	for _, scenario := range []string{"saved", "default", "override", "saved-over-override"} {
		t.Run(scenario, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			want := prompts.DefaultWikiPrompt()
			if strings.Contains(scenario, "override") {
				dir := t.TempDir()
				want = "決定と論点を分けて書く。"
				if err := os.WriteFile(filepath.Join(dir, prompts.DefaultPromptFile), []byte(want), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("KIOKU_PROMPTS_DIR", dir)
				rendered, err := prompts.Load(os.Getenv("KIOKU_PROMPTS_DIR"), prompts.Variables{}, MCPToolNames())
				if err != nil {
					t.Fatal(err)
				}
				f.s.Config.Prompts = rendered
				f.setRunners()
			}
			api := "/api/projects/" + p.ID + "/views/wiki"
			if strings.HasPrefix(scenario, "saved") {
				want = "決定事項は decisions ページに日付の新しい順で並べる。\n"
				if code := f.request("PATCH", api, token, map[string]string{"prompt": want}, nil); code != 200 {
					t.Fatal(code)
				}
			}
			var detail viewDetail
			if code := f.request("GET", api, token, nil, &detail); code != 200 || detail.Prompt != want {
				t.Fatalf("GET = %d: %+v", code, detail)
			}
			putFile(t, f, token, p.ID, "source.md", "Evidence")
			parent := queueTestViewGeneration(t, f, token, p)
			archive := wikiInputArchive(t, f, parent)
			input := readInput(t, archive)
			if input["WIKI.md"] != want {
				t.Fatalf("WIKI.md = %q, want %q", input["WIKI.md"], want)
			}
			if _, ok := input["VIEW.md"]; ok {
				t.Fatal("Wiki input contains VIEW.md")
			}
			systemSeen := false
			for _, req := range ma.Requests() {
				if req.Method == "POST" && req.Path == "/v1/sessions" {
					agent := req.Body["agent"].(map[string]any)
					system, _ := agent["system"].(string)
					if system != f.s.Config.Prompts.System() || !strings.HasSuffix(system, "\n\n"+prompts.WikiInstructionsParagraph) {
						t.Fatalf("unexpected runner system: %q", system)
					}
					systemSeen = true
				}
			}
			if !systemSeen {
				t.Fatal("runner system was not observed")
			}
			if code := f.request("PATCH", api, token, map[string]string{"prompt": "次回から変更する。"}, nil); code != 200 {
				t.Fatal(code)
			}
			if !bytes.Equal(archive, wikiInputArchive(t, f, parent)) {
				t.Fatal("PATCH changed the running input archive")
			}
			sendOutput(t, f, ma, parent, "Published Wiki")
			ma.SetStatus(parent.Run.Ref.ID, "idle", "end_turn", 50)
			if err := f.s.reconcileAgentJob(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			if err := f.stateStore.View(func(st model.State) error {
				for _, item := range st.Inbox {
					if item.ProjectID == p.ID {
						t.Error("published inbox was not consumed")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			child := nextTestViewTask(t, f, parent)
			childInput := readInput(t, wikiInputArchive(t, f, child))
			if _, ok := childInput["WIKI.md"]; ok {
				t.Fatal("View input contains WIKI.md")
			}
			if childInput["VIEW.md"] == "" || !strings.Contains(childInput["wiki/index.md"], "Published Wiki") {
				t.Fatal("View did not receive its prompt and published Wiki")
			}
			completeTestViewTask(t, f, ma, child, "<h1>Project map</h1>", 50)
			if err := f.s.advanceViewGeneration(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			var got model.Job
			if code := f.request("GET", "/api/projects/"+p.ID+"/jobs/"+parent.ID, token, nil, &got); code != 200 || got.Status != "completed" || got.WikiStatus != "completed" || len(got.Views) != 1 || got.Views[0].Slug != "project-map" || got.Views[0].Status != "completed" {
				t.Fatalf("job = %d: %+v", code, got)
			}
		})
	}
}

type wikiPromptReadCounter struct {
	blob.Store
	key   blob.Key
	reads int
}

func (b *wikiPromptReadCounter) Get(ctx context.Context, key blob.Key) ([]byte, error) {
	if key == b.key {
		b.reads++
	}
	return b.Store.Get(ctx, key)
}

func TestEmptyInboxDoesNotReadWikiPrompt(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "source.md", "Evidence")
	parent := queueTestViewGeneration(t, f, token, p)
	sendOutput(t, f, ma, parent, "Published Wiki")
	ma.SetStatus(parent.Run.Ref.ID, "idle", "end_turn", 200)
	if err := f.s.reconcileAgentJob(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	if err := f.s.advanceViewGeneration(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	if code := f.request("PATCH", "/api/projects/"+p.ID+"/views/wiki", token, map[string]string{"prompt": "Saved"}, nil); code != 200 {
		t.Fatal(code)
	}
	key := viewKey(p.ID, newWikiViewRecord(p.ID).ID, "view.json")
	if err := f.s.Blobs.Put(t.Context(), key, []byte("{")); err != nil {
		t.Fatal(err)
	}
	counter := &wikiPromptReadCounter{Store: f.s.Blobs, key: key}
	f.s.Blobs = counter
	parent = queueTestViewGeneration(t, f, token, p)
	if !parent.Unchanged || parent.Run != nil {
		t.Fatalf("empty inbox started Wiki: %+v", parent)
	}
	child := nextTestViewTask(t, f, parent)
	completeTestViewTask(t, f, ma, child, "<h1>Project map</h1>", 50)
	if err := f.s.advanceViewGeneration(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	var got model.Job
	if code := f.request("GET", "/api/projects/"+p.ID+"/jobs/"+parent.ID, token, nil, &got); code != 200 || got.Status != "completed" || !got.Unchanged || got.Views[0].Status != "completed" {
		t.Fatalf("job = %d: %+v", code, got)
	}
	if counter.reads != 0 {
		t.Fatalf("read corrupt Wiki settings %d times", counter.reads)
	}
}

func TestWikiPromptFailureMatchesMissingInboxBody(t *testing.T) {
	type outcome struct {
		Status, Error, WikiStatus string
		Released                  bool
	}
	var outcomes []outcome
	for _, corrupt := range []bool{false, true} {
		name := "missing inbox body"
		if corrupt {
			name = "corrupt Wiki settings"
		}
		t.Run(name, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			api := "/api/projects/" + p.ID + "/views/wiki"
			if code := f.request("PATCH", api, token, map[string]string{"prompt": "Saved"}, nil); code != 200 {
				t.Fatal(code)
			}
			putFile(t, f, token, p.ID, "source.md", "Evidence")
			var item model.InboxItem
			if err := f.stateStore.View(func(st model.State) error {
				for _, in := range st.Inbox {
					if in.ProjectID == p.ID {
						item = in
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			key := item.BodyKey()
			if corrupt {
				key = viewKey(p.ID, newWikiViewRecord(p.ID).ID, "view.json")
			}
			original, err := f.s.Blobs.Get(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				err = f.s.Blobs.Put(t.Context(), key, []byte("{"))
			} else {
				err = f.s.Blobs.Delete(t.Context(), key)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := f.s.Blobs.Get(t.Context(), key)
			if corrupt {
				_, readErr = f.s.readWikiViewLocked(t.Context(), p.ID)
			}
			if readErr == nil {
				t.Fatal("fixture must fail to read")
			}
			var parent model.Job
			if code := f.request("POST", "/api/projects/"+p.ID+"/generate", token, map[string]int{"budget_cents": 200}, &parent); code != 202 {
				t.Fatal(code)
			}
			if err := f.stateStore.Update(func(st *model.State) error {
				parent = st.Jobs[parent.ID]
				parent.Status, parent.WikiStatus = "running", "running"
				st.Jobs[parent.ID] = parent
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.s.startAgentJob(t.Context(), parent); !errors.Is(err, runner.ErrTemporary) {
				t.Fatalf("start error = %v, want stop confirmation pending", err)
			}
			parent = storedJob(t, f, parent.ID)
			var initial model.Job
			if code := f.request("GET", "/api/projects/"+p.ID+"/jobs/"+parent.ID, token, nil, &initial); code != 200 || initial.Status != "running" || initial.Error != "" || initial.WikiStatus != "running" {
				t.Fatalf("initial job = %d: %+v", code, initial)
			}
			if parent.Run.AbortReason != readErr.Error() {
				t.Fatalf("abort reason = %q, want %q", parent.Run.AbortReason, readErr)
			}
			if err := f.stateStore.Update(func(st *model.State) error {
				if st.Inbox[item.ID].RunID != parent.Run.Attempt.ID {
					t.Error("inbox was released before stop confirmation")
				}
				job := st.Jobs[parent.ID]
				job.Run.PreparedAt = time.Now().Add(-6 * time.Minute)
				st.Jobs[job.ID] = job
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.s.reconcileAgentJob(t.Context(), storedJob(t, f, parent.ID)); err != nil {
				t.Fatal(err)
			}
			var got model.Job
			if code := f.request("GET", "/api/projects/"+p.ID+"/jobs/"+parent.ID, token, nil, &got); code != 200 || got.Status != "failed" || got.WikiStatus != "failed" || got.Error != readErr.Error() || len(got.Views) != 0 {
				t.Fatalf("failed job = %d: %+v", code, got)
			}
			released := false
			if err := f.stateStore.View(func(st model.State) error { in, ok := st.Inbox[item.ID]; released = ok && in.RunID == ""; return nil }); err != nil {
				t.Fatal(err)
			}
			if !released {
				t.Fatal("inbox not released")
			}
			for _, req := range ma.Requests() {
				if req.Method == "POST" && req.Path == "/v1/sessions" {
					t.Fatal("runner started on input failure")
				}
			}
			t.Logf("initial=%s/%s error=%q; final=%s/%s error=%q; released=%v; no session started", initial.Status, initial.WikiStatus, initial.Error, got.Status, got.WikiStatus, got.Error, released)
			outcomes = append(outcomes, outcome{got.Status, "input read error", got.WikiStatus, released})
			if err := f.s.Blobs.Put(t.Context(), key, original); err != nil {
				t.Fatal(err)
			}
			retry := queueTestViewGeneration(t, f, token, p)
			if retry.Run == nil || retry.Run.Ref.ID == "" || len(retry.Run.InboxIDs) != 1 || retry.Run.InboxIDs[0] != item.ID {
				t.Fatalf("released item was not retried: %+v", retry)
			}
			sendOutput(t, f, ma, retry, "Retried Wiki")
			ma.SetStatus(retry.Run.Ref.ID, "idle", "end_turn", 200)
			if err := f.s.reconcileAgentJob(t.Context(), retry); err != nil {
				t.Fatal(err)
			}
			if storedJob(t, f, retry.ID).WikiStatus != "completed" {
				t.Fatal("retry did not publish")
			}
		})
	}
	if len(outcomes) != 2 || !reflect.DeepEqual(outcomes[0], outcomes[1]) {
		t.Fatalf("failure outcomes differ: %+v", outcomes)
	}
}
