package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

func TestInboxRunPinsAndConsumesOnlyPublishedItems(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	first := putFile(t, f, token, p.ID, "a.md", "First body")
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://example.com/private", "Source body")}}, nil); c != 202 {
		t.Fatal(c)
	}
	j := startManaged(t, f, token, p.ID, false)
	raw := ma.Credential(j.Run.Ref.Resources["vault_id"])
	c, b := agentRequest(t, f, j, raw, "GET", "input", nil)
	if c != 200 {
		t.Fatal(c)
	}
	input := readInput(t, b)
	if !strings.Contains(input["inbox/"+first.ID+".md"], "First body") || input["CHANGES.md"] != "" {
		t.Fatal(input)
	}
	second := putFile(t, f, token, p.ID, "later.md", "Arrived during run")
	sendOutput(t, f, ma, j, "Published")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" {
		t.Fatal(j)
	}
	f.db.View(func(st model.State) error {
		if len(st.Inbox) != 1 || st.Inbox[second.ID].RunID != "" {
			t.Fatal(st.Inbox)
		}
		return nil
	})
	next := startManaged(t, f, token, p.ID, false)
	c, b = agentRequest(t, f, next, ma.Credential(next.Run.Ref.Resources["vault_id"]), "GET", "input", nil)
	if c != 200 {
		t.Fatal(c)
	}
	input = readInput(t, b)
	if input["wiki/index.md"] == "" || input["wiki/log.md"] == "" || input["inbox/"+first.ID+".md"] != "" {
		t.Fatal(input)
	}
}

func TestInboxFailureReleasesAndRetryUsesCurrentWaitingItems(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	first := putFile(t, f, token, p.ID, "a.md", "First body")
	j := startManaged(t, f, token, p.ID, false)
	url := testSourceURL("a.md", "First body")
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: url}, nil); c != 202 {
		t.Fatal(c)
	}
	replacement := waitingInboxItem(t, f, p.ID, url)
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" {
		t.Fatal(j)
	}
	f.db.View(func(st model.State) error {
		if _, ok := st.Inbox[first.ID]; ok {
			t.Fatal("superseded item returned")
		}
		if st.Inbox[replacement.ID].RunID != "" {
			t.Fatal(st.Inbox)
		}
		return nil
	})
	var retry model.Job
	if c := f.request("POST", "/api/projects/"+p.ID+"/jobs/"+j.ID+"/retry", token, nil, &retry); c != 202 {
		t.Fatal(c)
	}
	f.db.Update(func(st *model.State) error {
		retry = st.Jobs[j.ID]
		retry.Status = "running"
		st.Jobs[j.ID] = retry
		return nil
	})
	if err := f.s.startAgentJob(t.Context(), retry); err != nil {
		t.Fatal(err)
	}
	retry = storedJob(t, f, j.ID)
	if len(retry.Run.InboxIDs) != 1 || retry.Run.InboxIDs[0] != replacement.ID || retry.Run.Attempt.ID == j.Run.Attempt.ID {
		t.Fatal(retry)
	}
}

func TestInboxEmptyAndOverLimitSelection(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	j := managedJob(t, f, token, p.ID, false)
	before := len(ma.Requests())
	if err := f.s.startAgentJob(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	j = storedJob(t, f, j.ID)
	if !j.Unchanged || j.Status != "completed" || len(ma.Requests()) != before {
		t.Fatal(j)
	}
	f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://example.com/waiting", "Source body")}}, nil)
	f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/removed"}, nil)
	f.db.Update(func(st *model.State) error {
		q := st.Projects[p.ID]
		q.Revision = "r"
		st.Projects[p.ID] = q
		st.Revisions["r"] = model.Revision{ID: "r", ProjectID: p.ID, StoredBytes: f.s.Config.MaxWikiBytes + 1}
		return nil
	})
	j = startManaged(t, f, token, p.ID, false)
	f.db.View(func(st model.State) error {
		for _, id := range j.Run.InboxIDs {
			if !st.Inbox[id].Remove {
				t.Fatal(st.Inbox[id])
			}
		}
		if len(j.Run.InboxIDs) != 1 {
			t.Fatal(j.Run)
		}
		return nil
	})
	sendOutput(t, f, ma, j, "smaller")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	j = reconcileManaged(t, f, j)
	if j.Status != "completed" {
		t.Fatal(j)
	}
}

type failWikiWrite struct {
	blob.Store
	failed bool
}

func (b *failWikiWrite) Put(ctx context.Context, key blob.Key, data []byte) error {
	if !b.failed && strings.Contains(string(key), "/wiki/") {
		b.failed = true
		return errors.New("interrupted publication")
	}
	return b.Store.Put(ctx, key, data)
}
func TestInterruptedPublicationKeepsInbox(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	item := putFile(t, f, token, p.ID, "a.md", "Body")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Ready")
	f.s.Blobs = &failWikiWrite{Store: f.s.Blobs}
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	j = reconcileManaged(t, f, j)
	if j.Status != "failed" {
		t.Fatal(j)
	}
	f.db.View(func(st model.State) error {
		if !st.Projects[p.ID].WikiIncomplete || st.Inbox[item.ID].ID == "" || st.Inbox[item.ID].RunID != "" {
			t.Fatal(st.Projects[p.ID], st.Inbox)
		}
		return nil
	})
}

func TestInboxScheduleOnlyEligibleWaitingItems(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	f.s.Config.GenerateEvery = time.Hour
	now := time.Now().UTC()
	f.s.scheduleGenerations(now)
	f.s.scheduleGenerations(now.Add(time.Hour))
	f.db.View(func(st model.State) error {
		if len(st.Jobs) != 0 {
			t.Fatal(st.Jobs)
		}
		return nil
	})
	putFile(t, f, token, p.ID, "a.md", "Body")
	f.s.scheduleGenerations(now.Add(2 * time.Hour))
	f.s.scheduleGenerations(now.Add(3 * time.Hour))
	f.db.View(func(st model.State) error {
		if len(st.Jobs) != 1 {
			t.Fatal(st.Jobs)
		}
		return nil
	})
}

func TestInboxFailurePrefersWaitingReplacementRegardlessOfClock(t *testing.T) {
	for _, offset := range []time.Duration{0, -time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			first := putFile(t, f, token, p.ID, "a.md", "Body")
			j := startManaged(t, f, token, p.ID, false)
			if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: testSourceURL("a.md", "Body")}, nil); c != 202 {
				t.Fatal(c)
			}
			replacement := waitingInboxItem(t, f, p.ID, testSourceURL("a.md", "Body"))
			if err := f.db.Update(func(st *model.State) error {
				item := st.Inbox[replacement.ID]
				item.Created = st.Inbox[first.ID].Created.Add(offset)
				st.Inbox[item.ID] = item
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
			if got := reconcileManaged(t, f, j); got.Status != "failed" {
				t.Fatal(got)
			}
			if err := f.db.View(func(st model.State) error {
				if len(st.Inbox) != 1 || !st.Inbox[replacement.ID].Remove {
					t.Fatalf("failed item conflicts with accepted replacement: %+v", st.Inbox)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type failAfterWikiDelete struct {
	blob.Store
	failed bool
}

func (b *failAfterWikiDelete) Delete(ctx context.Context, key blob.Key) error {
	if err := b.Store.Delete(ctx, key); err != nil {
		return err
	}
	if !b.failed && strings.Contains(string(key), "/wiki/source-") {
		b.failed = true
		return errors.New("lost deletion response during publication")
	}
	return nil
}

func TestInterruptedSourceDeletionCanGenerateAgain(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	putFile(t, f, token, p.ID, "a.md", "Body")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "First")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	if got := reconcileManaged(t, f, j); got.Status != "completed" {
		t.Fatal(got)
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: testSourceURL("a.md", "Body")}, nil); c != 202 {
		t.Fatal(c)
	}
	j = startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Removed")
	objects := f.s.Blobs
	f.s.Blobs = &failAfterWikiDelete{Store: objects}
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	if got := reconcileManaged(t, f, j); got.Status != "failed" {
		t.Fatal(got)
	}
	f.s.Blobs = objects
	j = startManaged(t, f, token, p.ID, false)
	if j.Run == nil || j.Run.Ref.ID == "" {
		t.Fatalf("publication failure made the inbox unrecoverable: %+v", j)
	}
	sendOutput(t, f, ma, j, "Recovered")
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	if got := reconcileManaged(t, f, j); got.Status != "completed" {
		t.Fatal(got)
	}
}

type unavailableWikiRead struct{ blob.Store }

func (b unavailableWikiRead) Get(context.Context, blob.Key) ([]byte, error) {
	return nil, errors.New("storage unavailable")
}

func TestGenerationWikiOnlyIgnoresMissingFilesDuringRecovery(t *testing.T) {
	f, _, p, _ := managedFixture(t)
	rev := model.Revision{Files: map[string]blob.Key{"wiki/source-missing.md": blob.Key("projects/" + p.ID + "/wiki/source-missing.md")}}
	if _, err := f.s.generationWiki(t.Context(), rev, false); err == nil {
		t.Fatal("missing file in a complete wiki was ignored")
	}
	if pages, err := f.s.generationWiki(t.Context(), rev, true); err != nil || len(pages) != 0 {
		t.Fatal(pages, err)
	}
	f.s.Blobs = unavailableWikiRead{f.s.Blobs}
	if _, err := f.s.generationWiki(t.Context(), rev, true); err == nil {
		t.Fatal("storage failure was ignored during recovery")
	}
}

type failAfterLargeWikiWrite struct{ blob.Store }

func (b failAfterLargeWikiWrite) Put(ctx context.Context, key blob.Key, data []byte) error {
	if strings.HasSuffix(string(key), "/wiki/z.md") {
		return errors.New("interrupted after writing an oversized page")
	}
	return b.Store.Put(ctx, key, data)
}

func TestIncompleteOversizedPublicationCanRetry(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "manual"
		if automatic {
			name = "scheduled"
		}
		t.Run(name, func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			f.s.Config.MaxWikiBytes = 1 << 20
			j := startManaged(t, f, token, p.ID, true)
			pages := map[string]string{
				"wiki/index.md": "# Index",
				"wiki/large.md": strings.Repeat("a", (1<<20)+100),
				"wiki/z.md":     "# Last page",
			}
			if c, b := agentRequest(t, f, j, ma.Credential(j.Run.Ref.Resources["vault_id"]), "PUT", "output", outputArchive(t, pages)); c != 200 {
				t.Fatalf("output %d: %s", c, b)
			}
			objects := f.s.Blobs
			f.s.Blobs = failAfterLargeWikiWrite{objects}
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
			if got := reconcileManaged(t, f, j); got.Status != "failed" {
				t.Fatal(got)
			}
			f.s.Blobs = objects
			restartManagedFixture(t, f)
			if err := f.db.View(func(st model.State) error {
				p := st.Projects[p.ID]
				if !p.WikiIncomplete || st.Revisions[p.Revision].StoredBytes <= f.s.Config.MaxWikiBytes {
					t.Fatal("publication did not stop over quota", p)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if automatic {
				f.s.Config.GenerateEvery = time.Hour
				now := time.Now().UTC()
				for _, at := range []time.Time{now, now.Add(time.Hour)} {
					if err := f.s.scheduleGenerations(at); err != nil {
						t.Fatal(err)
					}
				}
				j = model.Job{}
				if err := f.db.Update(func(st *model.State) error {
					for _, candidate := range st.Jobs {
						if candidate.ProjectID == p.ID && candidate.Status == "queued" {
							j = candidate
							j.Status = "running"
							st.Jobs[j.ID] = j
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if j.ID == "" {
					t.Fatal("partial publication prevented scheduled retry")
				}
			} else {
				j = managedJob(t, f, token, p.ID, false)
			}
			if err := f.s.startAgentJob(t.Context(), j); err != nil {
				t.Fatal(err)
			}
			j = storedJob(t, f, j.ID)
			if j.Run == nil || j.Status != "running" {
				t.Fatalf("partial publication prevented inbox retry: %+v", j)
			}
			sendOutput(t, f, ma, j, "Recovered")
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
			if got := reconcileManaged(t, f, j); got.Status != "completed" {
				t.Fatal(got)
			}
			if err := f.db.View(func(st model.State) error {
				rev := st.Revisions[st.Projects[p.ID].Revision]
				if rev.CompleteBytes != nil || len(st.Inbox) != 0 {
					t.Fatal("recovery did not finish", rev.CompleteBytes, st.Inbox)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInterruptedRemovalKeepsPublishedQuotaRestriction(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	f.s.Config.MaxWikiBytes = 1 << 20
	j := startManaged(t, f, token, p.ID, true)
	// This item arrives after pinning and stays waiting after the oversized publish.
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://example.com/waiting", "Source body")}}, nil); c != 202 {
		t.Fatal(c)
	}
	pages := map[string]string{"wiki/index.md": "# Index", "wiki/large.md": strings.Repeat("a", (1<<20)+100)}
	if c, b := agentRequest(t, f, j, ma.Credential(j.Run.Ref.Resources["vault_id"]), "PUT", "output", outputArchive(t, pages)); c != 200 {
		t.Fatalf("output %d: %s", c, b)
	}
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	if got := reconcileManaged(t, f, j); got.Status != "completed" {
		t.Fatal(got)
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/fixture"}, nil); c != 202 {
		t.Fatal(c)
	}
	j = startManaged(t, f, token, p.ID, false)
	pages["wiki/large.md"] = "# Smaller"
	pages["wiki/z.md"] = "# Last page"
	if c, b := agentRequest(t, f, j, ma.Credential(j.Run.Ref.Resources["vault_id"]), "PUT", "output", outputArchive(t, pages)); c != 200 {
		t.Fatalf("output %d: %s", c, b)
	}
	objects := f.s.Blobs
	f.s.Blobs = failAfterLargeWikiWrite{objects}
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	if got := reconcileManaged(t, f, j); got.Status != "failed" {
		t.Fatal(got)
	}
	f.s.Blobs = objects
	restartManagedFixture(t, f)
	j = startManaged(t, f, token, p.ID, false)
	if err := f.db.View(func(st model.State) error {
		if len(j.Run.InboxIDs) != 1 || !st.Inbox[j.Run.InboxIDs[0]].Remove {
			t.Fatal("partial removal bypassed the published quota", j.Run.InboxIDs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
