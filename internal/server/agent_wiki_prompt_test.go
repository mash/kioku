package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/prompts"
)

func wikiInputArchive(t *testing.T, f *fixture, job fixtureJob) []byte {
	t.Helper()
	data, err := f.s.Blobs.Get(t.Context(), fixtureJobKey(job, "input.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWikiPromptGeneration(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"saved", "default", "override", "saved-over-override"} {
		t.Run(scenario, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			want := prompts.DefaultWikiPrompt()
			if strings.Contains(scenario, "override") {
				want = "決定と論点を分けて書く。"
				rendered, err := loadPrompts(t, prompts.Variables{}, map[string]string{prompts.DefaultPromptFile: want})
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
					if system != f.s.Config.Prompts.FileSystem() || !strings.Contains(system, prompts.WikiInstructionsParagraph) {
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
			if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(parent)); err != nil {
				t.Fatal(err)
			}
			if err := f.stateStore.Read(context.Background(), func(st *store.Queries) error {
				for _, item := range fixtureInboxList(t, st, p.ID) {
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
			if err := f.s.storage.advanceViews(t.Context(), parent.ID); err != nil {
				t.Fatal(err)
			}
			var got fixtureJob
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
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "source.md", "Evidence")
	parent := queueTestViewGeneration(t, f, token, p)
	sendOutput(t, f, ma, parent, "Published Wiki")
	ma.SetStatus(parent.Run.Ref.ID, "idle", "end_turn", 200)
	if err := f.s.reconcileAgentJob(t.Context(), fixtureRow(parent)); err != nil {
		t.Fatal(err)
	}
	if err := f.s.storage.advanceViews(t.Context(), parent.ID); err != nil {
		t.Fatal(err)
	}
	if code := f.request("PATCH", "/api/projects/"+p.ID+"/views/wiki", token, map[string]string{"prompt": "Saved"}, nil); code != 200 {
		t.Fatal(code)
	}
	key := viewKey(p.ID, newWikiViewRow(p.ID).ID, "view.json")
	if err := f.s.Blobs.Put(t.Context(), key, []byte("{")); err != nil {
		t.Fatal(err)
	}
	counter := &wikiPromptReadCounter{Store: f.s.Blobs, key: key}
	f.s.Blobs = counter
	f.s.storage.objects = f.s.Blobs
	parent = queueTestViewGeneration(t, f, token, p)
	if !parent.Unchanged || parent.Run != nil {
		t.Fatalf("empty inbox started Wiki: %+v", parent)
	}
	child := nextTestViewTask(t, f, parent)
	completeTestViewTask(t, f, ma, child, "<h1>Project map</h1>", 50)
	if err := f.s.storage.advanceViews(t.Context(), parent.ID); err != nil {
		t.Fatal(err)
	}
	var got fixtureJob
	if code := f.request("GET", "/api/projects/"+p.ID+"/jobs/"+parent.ID, token, nil, &got); code != 200 || got.Status != "completed" || !got.Unchanged || got.Views[0].Status != "completed" {
		t.Fatalf("job = %d: %+v", code, got)
	}
	if counter.reads != 0 {
		t.Fatalf("read corrupt Wiki settings %d times", counter.reads)
	}
}

// E13: input preparation failure precedes Run creation and inbox assignment.
func TestMissingInboxBodyFailsBeforeStartingRun(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	item := putFile(t, f, token, p.ID, "source.md", "Evidence")
	body, err := f.s.Blobs.Get(t.Context(), item.BodyKey())
	fixtureCheck(t, err)
	fixtureCheck(t, f.s.Blobs.Delete(t.Context(), item.BodyKey()))
	parent := managedJob(t, f, token, p.ID, false)
	fixtureCheck(t, f.s.startAgentJob(t.Context(), fixtureRow(parent)))
	parent = storedJob(t, f, parent.ID)
	if parent.Status != "failed" || parent.Run != nil || parent.Error == "" || parent.WikiStatus != "failed" {
		t.Fatal(parent)
	}
	if item := fixtureInboxGet(t, f, item.ID); item.RunID != "" {
		t.Fatal(item)
	}
	fixtureCheck(t, f.stateStore.Read(t.Context(), func(q *store.Queries) error {
		_, e := q.Usage(parent.ID)
		if !errors.Is(e, sql.ErrNoRows) {
			t.Fatal("usage without Run", e)
		}
		return nil
	}))
	for _, req := range ma.Requests() {
		if req.Method == "POST" && req.Path == "/v1/sessions" {
			t.Fatal("provider started before input ready")
		}
	}
	fixtureCheck(t, f.s.Blobs.Put(t.Context(), item.BodyKey(), body))
	retry := startManaged(t, f, token, p.ID, false)
	if retry.Run == nil || len(retry.Run.InboxIDs) != 1 || retry.Run.InboxIDs[0] != item.ID {
		t.Fatal(retry)
	}
	sendOutput(t, f, ma, retry, "Retried Wiki")
	ma.SetStatus(retry.Run.Ref.ID, "idle", "end_turn", 200)
	if got := reconcileManaged(t, f, retry); got.Status != "completed" {
		t.Fatal(got)
	}
}
