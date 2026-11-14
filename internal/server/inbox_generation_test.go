package server

import (
	"context"
	"errors"
	"fmt"
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
	f.stateStore.View(func(st model.State) error {
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
	f.stateStore.View(func(st model.State) error {
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
	f.stateStore.Update(func(st *model.State) error {
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
	f.stateStore.Update(func(st *model.State) error {
		q := st.Projects[p.ID]
		q.Revision = "r"
		st.Projects[p.ID] = q
		st.Revisions["r"] = model.Revision{ID: "r", ProjectID: p.ID, StoredBytes: f.s.Config.MaxWikiBytes + 1}
		return nil
	})
	j = startManaged(t, f, token, p.ID, false)
	f.stateStore.View(func(st model.State) error {
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
	f.stateStore.View(func(st model.State) error {
		if !st.Projects[p.ID].WikiIncomplete || st.Inbox[item.ID].ID == "" || st.Inbox[item.ID].RunID != "" {
			t.Fatal(st.Projects[p.ID], st.Inbox)
		}
		return nil
	})
}

func TestInboxScheduleOnlyEligibleWaitingItems(t *testing.T) {
	for _, tc := range []struct {
		name                                          string
		item, remove, pinned, over, noOwner, deleting bool
		jobs                                          int
	}{
		{name: "empty"},
		{name: "waiting", item: true, jobs: 1},
		{name: "pinned", item: true, pinned: true},
		{name: "oversized", item: true, over: true},
		{name: "removal while oversized", item: true, remove: true, over: true, jobs: 1},
		{name: "no owner", item: true, noOwner: true},
		{name: "deleting", item: true, deleting: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, p, _ := managedFixture(t)
			f.s.Config.GenerateDebounce = time.Hour
			now := time.Now().UTC()
			generationTick(t, f, now)
			if got := generationState(t, f).Projects[p.ID].NextGenerateAt; !got.IsZero() {
				t.Error("tick created deadline without a put")
			}
			if err := f.stateStore.Update(func(st *model.State) error {
				p := st.Projects[p.ID]
				p.NextGenerateAt = now
				p.Deleting = tc.deleting
				if tc.noOwner {
					p.Members = map[string]string{}
				}
				if tc.over {
					p.Revision = "large"
					st.Revisions["large"] = model.Revision{ID: "large", ProjectID: p.ID, StoredBytes: f.s.Config.MaxWikiBytes + 1}
				}
				st.Projects[p.ID] = p
				if tc.item {
					item := model.InboxItem{ID: "item", ProjectID: p.ID, Remove: tc.remove}
					if tc.pinned {
						item.RunID = "run"
					}
					st.Inbox[item.ID] = item
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			generationTick(t, f, now)
			st := generationState(t, f)
			if len(st.Jobs) != tc.jobs {
				t.Fatal(st.Jobs)
			}
			if got := len(f.s.wake); got != tc.jobs {
				t.Fatalf("worker notifications = %d, queued jobs = %d", got, tc.jobs)
			}
			if tc.deleting {
				if !st.Projects[p.ID].NextGenerateAt.Equal(now) {
					t.Fatal("deleting deadline changed")
				}
			} else if !st.Projects[p.ID].NextGenerateAt.IsZero() {
				t.Fatal("deadline was not cleared")
			}
		})
	}
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
			if err := f.stateStore.Update(func(st *model.State) error {
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
			if err := f.stateStore.View(func(st model.State) error {
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
			f.s.Config.GenerateDebounce = 10 * time.Second
			j := startManaged(t, f, token, p.ID, true)
			if automatic {
				putFile(t, f, token, p.ID, "during.md", "Arrived during generation")
				deadline := generationState(t, f).Projects[p.ID].NextGenerateAt
				generationTick(t, f, deadline)
				if got := generationState(t, f).Projects[p.ID].NextGenerateAt; !got.Equal(deadline) {
					t.Fatal("running job did not preserve deadline")
				}
			}
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
			if err := f.stateStore.View(func(st model.State) error {
				p := st.Projects[p.ID]
				if !p.WikiIncomplete || st.Revisions[p.Revision].StoredBytes <= f.s.Config.MaxWikiBytes {
					t.Fatal("publication did not stop over quota", p)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if automatic {
				generationTick(t, f, generationState(t, f).Projects[p.ID].NextGenerateAt)
				j = model.Job{}
				if err := f.stateStore.Update(func(st *model.State) error {
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
			if automatic && len(j.Run.InboxIDs) != 2 {
				t.Fatal("retry must include the released and newly waiting items", j.Run.InboxIDs)
			}
			sendOutput(t, f, ma, j, "Recovered")
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
			if got := reconcileManaged(t, f, j); got.Status != "completed" {
				t.Fatal(got)
			}
			if err := f.stateStore.View(func(st model.State) error {
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
	if err := f.stateStore.View(func(st model.State) error {
		if len(j.Run.InboxIDs) != 1 || !st.Inbox[j.Run.InboxIDs[0]].Remove {
			t.Fatal("partial removal bypassed the published quota", j.Run.InboxIDs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Record the end of blob storage so the test distinguishes save time from
// request arrival (InboxItem.Created).
type deadlineSaveBlob struct {
	blob.Store
	savedAfter time.Time
}

func (b *deadlineSaveBlob) Put(ctx context.Context, key blob.Key, data []byte) error {
	if err := b.Store.Put(ctx, key, data); err != nil {
		return err
	}
	b.savedAfter = time.Now().UTC()
	return nil
}

func generationState(t *testing.T, f *fixture) model.State {
	t.Helper()
	var state model.State
	if err := f.stateStore.View(func(st model.State) error { state = st; return nil }); err != nil {
		t.Fatal(err)
	}
	return state
}

func generationTick(t *testing.T, f *fixture, now time.Time) {
	t.Helper()
	if err := f.s.scheduleGenerations(now); err != nil {
		t.Fatal(err)
	}
}

func TestPutSetsDeadlineFromSaveTime(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	f.s.Config.GenerateDebounce = 10 * time.Second
	objects := &deadlineSaveBlob{Store: f.s.Blobs}
	f.s.Blobs = objects
	for i := 0; i < 2; i++ {
		previous := generationState(t, f).Projects[p.ID].NextGenerateAt
		item := putFile(t, f, token, p.ID, "a.md", "Body")
		deadline := generationState(t, f).Projects[p.ID].NextGenerateAt
		if !objects.savedAfter.After(item.Created) || deadline.Before(objects.savedAfter.Add(10*time.Second)) || deadline.After(time.Now().UTC().Add(10*time.Second)) || !deadline.After(previous) {
			t.Fatalf("created=%v savedAfter=%v deadline=%v previous=%v", item.Created, objects.savedAfter, deadline, previous)
		}
	}
	before := time.Now().UTC()
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/remove"}, nil); c != 202 {
		t.Fatal(c)
	}
	deadline := generationState(t, f).Projects[p.ID].NextGenerateAt
	if deadline.Before(before.Add(10*time.Second)) || deadline.After(time.Now().UTC().Add(10*time.Second)) {
		t.Fatal(deadline)
	}
	f.s.Blobs = objects.Store
	restartManagedFixture(t, f)
	if got := generationState(t, f).Projects[p.ID].NextGenerateAt; !got.Equal(deadline) {
		t.Fatalf("deadline not persisted: %v", got)
	}
}

func TestRejectedPutKeepsDeadline(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprint(deleting), func(t *testing.T) {
			f, token, p, _ := managedFixture(t)
			f.s.Config.GenerateDebounce = 10 * time.Second
			putFile(t, f, token, p.ID, "a.md", "Body")
			before := generationState(t, f)
			deadline := before.Projects[p.ID].NextGenerateAt
			if err := f.stateStore.Update(func(st *model.State) error {
				p := st.Projects[p.ID]
				p.Deleting, p.Revision = deleting, "large"
				st.Projects[p.ID] = p
				st.Revisions["large"] = model.Revision{ID: "large", ProjectID: p.ID, StoredBytes: f.s.Config.MaxWikiBytes + 1}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			want := 413
			if deleting {
				want = 409
			}
			if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "new.md", Content: testSourceContent("https://example.com/new", "New")}}, nil); c != want {
				t.Fatal(c)
			}
			after := generationState(t, f)
			if !after.Projects[p.ID].NextGenerateAt.Equal(deadline) || len(after.Inbox) != len(before.Inbox) {
				t.Fatal("rejected put changed deadline or inbox")
			}
		})
	}
}

func TestDebounceChangeAppliesFromNextPut(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	f.s.Config.GenerateDebounce = 10 * time.Second
	putFile(t, f, token, p.ID, "a.md", "Body")
	old := generationState(t, f).Projects[p.ID].NextGenerateAt
	// An expired stored deadline survives restarting with a different setting.
	old = old.Add(-time.Minute)
	if err := f.stateStore.Update(func(st *model.State) error {
		p := st.Projects[p.ID]
		p.NextGenerateAt = old
		st.Projects[p.ID] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.s.Config.GenerateDebounce = 30 * time.Second
	restartManagedFixture(t, f)
	if got := generationState(t, f).Projects[p.ID].NextGenerateAt; !got.Equal(old) {
		t.Fatal(got)
	}
	before := time.Now().UTC()
	putFile(t, f, token, p.ID, "b.md", "Body")
	got := generationState(t, f).Projects[p.ID].NextGenerateAt
	if got.Before(before.Add(30*time.Second)) || got.After(time.Now().UTC().Add(30*time.Second)) {
		t.Fatal(got)
	}
}

func TestDeadlineQueuesOnceAndClears(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	f.s.Config.GenerateDebounce = 10 * time.Second
	putFile(t, f, token, p.ID, "a.md", "Body")
	deadline := generationState(t, f).Projects[p.ID].NextGenerateAt
	generationTick(t, f, deadline.Add(-time.Nanosecond))
	if st := generationState(t, f); len(st.Jobs) != 0 || !st.Projects[p.ID].NextGenerateAt.Equal(deadline) {
		t.Fatal("early tick changed state")
	}
	generationTick(t, f, deadline)
	for _, at := range []time.Time{deadline, deadline.Add(24 * time.Hour)} {
		generationTick(t, f, at)
	}
	st := generationState(t, f)
	if !st.Projects[p.ID].NextGenerateAt.IsZero() || len(st.Jobs) != 1 {
		t.Fatal(st.Projects[p.ID], st.Jobs)
	}
	for _, j := range st.Jobs {
		if !j.Automatic || j.Status != "queued" || p.Members[j.UserID] != "owner" || !j.Created.Equal(deadline) {
			t.Fatal(j)
		}
	}
}

func TestDeadlineWaitsForJob(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		t.Run(status, func(t *testing.T) {
			f, token, p, _ := managedFixture(t)
			f.s.Config.GenerateDebounce = 10 * time.Second
			var job model.Job
			if c := f.request("POST", "/api/projects/"+p.ID+"/generate", token, nil, &job); c != 202 {
				t.Fatal(c)
			}
			if err := f.stateStore.Update(func(st *model.State) error { j := st.Jobs[job.ID]; j.Status = status; st.Jobs[j.ID] = j; return nil }); err != nil {
				t.Fatal(err)
			}
			putFile(t, f, token, p.ID, "a.md", "Body")
			deadline := generationState(t, f).Projects[p.ID].NextGenerateAt
			select {
			case <-f.s.wake: // Consume the manual generation's notification.
			default:
				t.Fatal("manual generation did not notify worker")
			}
			generationTick(t, f, deadline)
			if st := generationState(t, f); !st.Projects[p.ID].NextGenerateAt.Equal(deadline) || len(st.Jobs) != 1 {
				t.Fatal("job did not preserve deadline")
			}
			if len(f.s.wake) != 0 {
				t.Fatal("waiting deadline woke worker without queueing a job")
			}
			putFile(t, f, token, p.ID, "b.md", "Body")
			extended := generationState(t, f).Projects[p.ID].NextGenerateAt
			if !extended.After(deadline) {
				t.Fatal("put did not extend waiting deadline")
			}
			if err := f.stateStore.Update(func(st *model.State) error {
				j := st.Jobs[job.ID]
				j.Status = "completed"
				st.Jobs[j.ID] = j
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			generationTick(t, f, deadline)
			if st := generationState(t, f); len(st.Jobs) != 1 {
				t.Fatal("queued before extended deadline")
			}
			generationTick(t, f, extended)
			if st := generationState(t, f); !st.Projects[p.ID].NextGenerateAt.IsZero() || len(st.Jobs) != 2 {
				t.Fatal(st.Jobs)
			}
		})
	}
}

func TestFailedAutomaticJobIsNotRequeued(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	f.s.Config.GenerateDebounce = 10 * time.Second
	first := putFile(t, f, token, p.ID, "a.md", "Body")
	generationTick(t, f, generationState(t, f).Projects[p.ID].NextGenerateAt)
	var job model.Job
	if err := f.stateStore.Update(func(st *model.State) error {
		for _, j := range st.Jobs {
			job = j
			job.Status = "running"
			st.Jobs[job.ID] = job
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !job.Automatic {
		t.Fatal(job)
	}
	if err := f.s.startAgentJob(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	job = storedJob(t, f, job.ID)
	ma.SetStatus(job.Run.Ref.ID, "idle", "end_turn", 1)
	if got := reconcileManaged(t, f, job); got.Status != "failed" {
		t.Fatal(got)
	}
	for _, at := range []time.Time{time.Now().Add(time.Hour), time.Now().Add(48 * time.Hour)} {
		generationTick(t, f, at)
	}
	st := generationState(t, f)
	if len(st.Jobs) != 1 || st.Inbox[first.ID].ID == "" || st.Inbox[first.ID].RunID != "" || !st.Projects[p.ID].NextGenerateAt.IsZero() {
		t.Fatal("failed job was retried or did not release inbox")
	}
	putFile(t, f, token, p.ID, "b.md", "Body")
	generationTick(t, f, generationState(t, f).Projects[p.ID].NextGenerateAt)
	if st := generationState(t, f); len(st.Jobs) != 2 {
		t.Fatal("new put did not schedule generation")
	}
}

func TestZeroDebounceQueuesOnNextTick(t *testing.T) {
	for _, debounce := range []time.Duration{0, time.Nanosecond} {
		t.Run(debounce.String(), func(t *testing.T) {
			f, token, p, _ := managedFixture(t)
			f.s.Config.GenerateDebounce = debounce
			before := time.Now().UTC()
			putFile(t, f, token, p.ID, "a.md", "Body")
			st := generationState(t, f)
			if len(st.Jobs) != 0 || st.Projects[p.ID].NextGenerateAt.Before(before) {
				t.Fatal("put must only set the deadline")
			}
			generationTick(t, f, time.Now().UTC())
			st = generationState(t, f)
			if len(st.Jobs) != 1 || !st.Projects[p.ID].NextGenerateAt.IsZero() {
				t.Fatal(st.Jobs)
			}
		})
	}
}

func TestStoredFixedDeadlineFiresOnce(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	putFile(t, f, token, p.ID, "a.md", "Body")
	deadline := time.Now().UTC().Add(time.Hour)
	if err := f.stateStore.Update(func(st *model.State) error {
		p := st.Projects[p.ID]
		p.NextGenerateAt = deadline
		st.Projects[p.ID] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.s.Config.GenerateDebounce = 10 * time.Second
	restartManagedFixture(t, f)
	generationTick(t, f, deadline.Add(-time.Nanosecond))
	if st := generationState(t, f); len(st.Jobs) != 0 || !st.Projects[p.ID].NextGenerateAt.Equal(deadline) {
		t.Fatal("stored deadline changed")
	}
	generationTick(t, f, deadline)
	if err := f.stateStore.Update(func(st *model.State) error {
		for id, j := range st.Jobs {
			j.Status = "completed"
			st.Jobs[id] = j
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	generationTick(t, f, deadline.Add(48*time.Hour))
	if st := generationState(t, f); len(st.Jobs) != 1 || !st.Projects[p.ID].NextGenerateAt.IsZero() {
		t.Fatal("fixed deadline repeated")
	}
}

func TestFullInboxLetsDeadlinePass(t *testing.T) {
	f, token, p, _ := managedFixture(t)
	f.s.Config.GenerateDebounce = 10 * time.Second
	putFile(t, f, token, p.ID, "a.md", "Body")
	deadline := generationState(t, f).Projects[p.ID].NextGenerateAt
	if err := f.stateStore.Update(func(st *model.State) error {
		for i := 1; i < maxInboxItems; i++ {
			id := fmt.Sprintf("item%d", i)
			st.Inbox[id] = model.InboxItem{ID: id, ProjectID: p.ID, SourceURL: "https://example.com/" + id, Remove: true}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/full"}, nil); c != 429 {
		t.Fatal(c)
	}
	if st := generationState(t, f); !st.Projects[p.ID].NextGenerateAt.Equal(deadline) || len(st.Inbox) != maxInboxItems {
		t.Fatal("rejected put changed state")
	}
	generationTick(t, f, deadline)
	if st := generationState(t, f); len(st.Jobs) != 1 || !st.Projects[p.ID].NextGenerateAt.IsZero() {
		t.Fatal(st.Jobs)
	}
}

func TestIdleTickDoesNotCloneState(t *testing.T) {
	f, _, p, _ := managedFixture(t)
	now := time.Now().UTC()
	for _, tc := range []struct {
		name     string
		deadline time.Time
		deleting bool
	}{
		{"no deadline", time.Time{}, false},
		{"future deadline", now.Add(time.Hour), false},
		{"deleting", now.Add(-time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := f.stateStore.Update(func(st *model.State) error {
				p := st.Projects[p.ID]
				p.NextGenerateAt = tc.deadline
				p.Deleting = tc.deleting
				st.Projects[p.ID] = p
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if allocs := testing.AllocsPerRun(100, func() { generationTick(t, f, now) }); allocs != 0 {
				t.Fatalf("idle tick allocated %v times", allocs)
			}
			if got := generationState(t, f).Projects[p.ID].NextGenerateAt; !got.Equal(tc.deadline) {
				t.Fatal("idle tick changed deadline")
			}
		})
	}
}
