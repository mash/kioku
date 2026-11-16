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
	"github.com/mash/kioku/internal/store"
)

func TestInboxRunPinsAndConsumesOnlyPublishedItems(t *testing.T) {
	t.Parallel()
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
	f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if len(fixtureInboxList(t, st, p.ID)) != 1 || fixtureInboxGet(t, st, second.ID).RunID != "" {
			t.Fatal(fixtureInboxList(t, st, p.ID))
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

func TestInboxFailureReleasesForNewGeneration(t *testing.T) {
	t.Parallel()
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
	f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if _, ok := fixtureInboxFind(t, st, first.ID); ok {
			t.Fatal("superseded item returned")
		}
		if fixtureInboxGet(t, st, replacement.ID).RunID != "" {
			t.Fatal(fixtureInboxList(t, st, p.ID))
		}
		return nil
	})
	next := startManaged(t, f, token, p.ID, false)
	if next.ID == j.ID || len(next.Run.InboxIDs) != 1 || next.Run.InboxIDs[0] != replacement.ID || next.Run.Attempt.ID == j.Run.Attempt.ID {
		t.Fatal(next)
	}
	if failed := storedJob(t, f, j.ID); failed.Status != "failed" {
		t.Fatal("the previous job lost its failure record", failed)
	}
}

func TestInboxEmptyAndOverLimitSelection(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	j := managedJob(t, f, token, p.ID, false)
	before := len(ma.Requests())
	if err := f.s.startAgentJob(t.Context(), fixtureRow(j)); err != nil {
		t.Fatal(err)
	}
	j = f.completeFixtureViews(storedJob(t, f, j.ID))
	if !j.Unchanged || j.Status != "completed" || len(ma.Requests()) != before {
		t.Fatal(j)
	}
	f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://example.com/waiting", "Source body")}}, nil)
	f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/removed"}, nil)
	f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		q := fixtureProjectsGet(t, st, p.ID)
		q.Revision = "r"
		fixtureProjectsPut(t, st, p.ID, q)
		fixtureOversizedWiki(t, f, st, p.ID, "r")
		return nil
	})
	j = startManaged(t, f, token, p.ID, false)
	f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		for _, id := range j.Run.InboxIDs {
			if !fixtureInboxGet(t, st, id).Remove {
				t.Fatal(fixtureInboxGet(t, st, id))
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
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	item := putFile(t, f, token, p.ID, "a.md", "Body")
	j := startManaged(t, f, token, p.ID, false)
	sendOutput(t, f, ma, j, "Ready")
	f.s.Blobs = &failWikiWrite{Store: f.s.Blobs}
	f.s.storage.objects = f.s.Blobs
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	j = deferredPublication(t, f, j)
	if j.Status != "running" {
		t.Fatal(j)
	}
	f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		if fixtureProjectsGet(t, st, p.ID).WikiIncomplete || fixtureInboxGet(t, st, item.ID).ID == "" || fixtureInboxGet(t, st, item.ID).RunID != j.ID {
			t.Fatal(fixtureProjectsGet(t, st, p.ID), fixtureInboxList(t, st, p.ID))
		}
		return nil
	})
	if got := reconcileManaged(t, f, j); got.Status != "completed" {
		t.Fatal(got)
	}
}

func TestInboxScheduleOnlyEligibleWaitingItems(t *testing.T) {
	t.Parallel()
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
			now := time.Now().UTC().Truncate(time.Millisecond)
			generationTick(t, f, now)
			if got := fixtureProjectsGet(t, f, p.ID).NextGenerateAt; !got.IsZero() {
				t.Error("tick created deadline without a put")
			}
			if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				p := fixtureProjectsGet(t, st, p.ID)
				p.NextGenerateAt = now
				p.Deleting = tc.deleting
				if tc.noOwner {
					p.Members = map[string]string{}
				}
				if tc.over {
					p.Revision = "large"
					fixtureOversizedWiki(t, f, st, p.ID, "large")
				}
				fixtureProjectsPut(t, st, p.ID, p)
				if tc.item {
					item := model.InboxItem{ID: "item", ProjectID: p.ID, Remove: tc.remove}
					if tc.pinned {
						item.RunID = "run"
					}
					fixtureInboxPut(t, st, item.ID, item)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			generationTick(t, f, now)
			st := f
			if len(fixtureJobsList(t, st, p.ID)) != tc.jobs {
				t.Fatal(fixtureJobsList(t, st, p.ID))
			}
			if got := len(f.s.wake); got != tc.jobs {
				t.Fatalf("worker notifications = %d, queued jobs = %d", got, tc.jobs)
			}
			if tc.deleting {
				if !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.Equal(now) {
					t.Fatal("deleting deadline changed")
				}
			} else if !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.IsZero() {
				t.Fatal("deadline was not cleared")
			}
		})
	}
}

func TestInboxFailurePrefersWaitingReplacementRegardlessOfClock(t *testing.T) {
	t.Parallel()
	for _, offset := range []time.Duration{0, -time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			f, token, p, ma := managedFixture(t)
			first := putFile(t, f, token, p.ID, "a.md", "Body")
			j := startManaged(t, f, token, p.ID, false)
			if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: testSourceURL("a.md", "Body")}, nil); c != 202 {
				t.Fatal(c)
			}
			replacement := waitingInboxItem(t, f, p.ID, testSourceURL("a.md", "Body"))
			if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				item := fixtureInboxGet(t, st, replacement.ID)
				item.Created = fixtureInboxGet(t, st, first.ID).Created.Add(offset)
				fixtureInboxPut(t, st, item.ID, item)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
			if got := reconcileManaged(t, f, j); got.Status != "failed" {
				t.Fatal(got)
			}
			if err := f.stateStore.Read(context.Background(), func(st *store.Queries) error {
				if len(fixtureInboxList(t, st, p.ID)) != 1 || !fixtureInboxGet(t, st, replacement.ID).Remove {
					t.Fatalf("failed item conflicts with accepted replacement: %+v", fixtureInboxList(t, st, p.ID))
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
	if !b.failed && strings.Contains(string(key), "/.kioku/wiki/") {
		b.failed = true
		return errors.New("lost deletion response during publication")
	}
	return nil
}

func TestInterruptedSourceDeletionCanGenerateAgain(t *testing.T) {
	t.Parallel()
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
	f.s.storage.objects = f.s.Blobs
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	if got := reconcileManaged(t, f, j); got.Status != "completed" {
		t.Fatal(got)
	}
	// E1/E5: source removal clears SQL references; object deletion is deferred to GC.
	if _, err := f.s.storage.collectGarbage(t.Context(), p.ID); err == nil {
		t.Fatal("expected interrupted GC")
	}
	f.s.Blobs = objects
	f.s.storage.objects = objects
	if _, err := f.s.storage.collectGarbage(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	if got := startManaged(t, f, token, p.ID, false); !got.Unchanged {
		t.Fatal(got)
	}
}

type unavailableWikiRead struct{ blob.Store }

func (b unavailableWikiRead) Get(context.Context, blob.Key) ([]byte, error) {
	return nil, errors.New("storage unavailable")
}

// E1: missing immutable bodies are failures, including incomplete publication.
func TestGenerationWikiRejectsMissingImmutableFiles(t *testing.T) {
	t.Parallel()
	f, _, p, _ := managedFixture(t)
	rev := model.Revision{Files: map[string]blob.Key{"wiki/source-missing.md": model.WikiContentKey(p.ID, model.ContentHash(strings.Repeat("a", 64)))}}
	if _, err := f.s.generationWiki(t.Context(), rev); err == nil {
		t.Fatal("missing body ignored")
	}
	f.s.Blobs = unavailableWikiRead{f.s.Blobs}
	f.s.storage.objects = f.s.Blobs
	if _, err := f.s.generationWiki(t.Context(), rev); err == nil {
		t.Fatal("storage failure ignored")
	}
}

type failAfterLargeWikiWrite struct{ blob.Store }

func (b failAfterLargeWikiWrite) Put(ctx context.Context, key blob.Key, data []byte) error {
	if strings.Contains(string(key), "/.kioku/wiki/") && string(data) == "# Last page" {
		return errors.New("interrupted after writing an oversized page")
	}
	return b.Store.Put(ctx, key, data)
}

func TestIncompleteOversizedPublicationCanRetry(t *testing.T) {
	t.Parallel()
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
				deadline := fixtureProjectsGet(t, f, p.ID).NextGenerateAt
				generationTick(t, f, deadline)
				if got := fixtureProjectsGet(t, f, p.ID).NextGenerateAt; !got.Equal(deadline) {
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
			f.s.storage.objects = f.s.Blobs
			ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
			j = deferredPublication(t, f, j)
			f.s.Blobs = objects
			f.s.storage.objects = f.s.Blobs
			restartManagedFixture(t, f)
			if err := f.stateStore.Read(context.Background(), func(st *store.Queries) error {
				p := fixtureProjectsGet(t, st, p.ID)
				if p.WikiIncomplete || p.Revision != "" || len(fixtureRevisionsList(t, st, p.ID)) != 0 {
					t.Fatal("failed upload changed published Wiki", p)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// E1/E14: retry the saved output of the same running job.
			if automatic {
				generationTick(t, f, fixtureProjectsGet(t, f, p.ID).NextGenerateAt)
			}
			if got := reconcileManaged(t, f, j); got.Status != "completed" {
				t.Fatal(got)
			}
			fixtureCheck(t, f.stateStore.Read(t.Context(), func(q *store.Queries) error {
				project, err := q.Project(p.ID)
				if err != nil {
					return err
				}
				items, err := q.ProjectInbox(p.ID)
				want := 0
				if automatic {
					want = 1
				}
				if len(items) != want {
					t.Fatal("recovery lost inbox boundaries", project, items)
				}
				return err
			}))
		})
	}
}

func TestInterruptedRemovalKeepsPublishedQuotaRestriction(t *testing.T) {
	t.Parallel()
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
	f.s.storage.objects = f.s.Blobs
	ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
	j = deferredPublication(t, f, j)
	if code := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "new.md", Content: testSourceContent("https://example.com/new", "Body")}}, nil); code != 413 {
		t.Fatal("partial removal bypassed completed quota", code)
	}
	if len(j.Run.InboxIDs) != 1 || !fixtureInboxGet(t, f, j.Run.InboxIDs[0]).Remove {
		t.Fatal(j.Run.InboxIDs)
	}
	f.s.Blobs = objects
	f.s.storage.objects = objects
	restartManagedFixture(t, f)
	if got := reconcileManaged(t, f, j); got.Status != "completed" {
		t.Fatal(got)
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
	b.savedAfter = time.Now().UTC().Truncate(time.Millisecond)
	return nil
}

func generationTick(t *testing.T, f *fixture, now time.Time) {
	t.Helper()
	if err := f.s.scheduleGenerations(now); err != nil {
		t.Fatal(err)
	}
}

func TestPutSetsDeadlineFromSaveTime(t *testing.T) {
	t.Parallel()
	f, token, p, _ := managedFixture(t)
	f.s.Config.GenerateDebounce = 10 * time.Second
	objects := &deadlineSaveBlob{Store: f.s.Blobs}
	f.s.Blobs = objects
	f.s.storage.objects = f.s.Blobs
	for i := 0; i < 2; i++ {
		previous := fixtureProjectsGet(t, f, p.ID).NextGenerateAt
		item := putFile(t, f, token, p.ID, "a.md", "Body")
		deadline := fixtureProjectsGet(t, f, p.ID).NextGenerateAt
		if !objects.savedAfter.After(item.Created) || deadline.Before(objects.savedAfter.Truncate(time.Millisecond).Add(10*time.Second)) || deadline.After(time.Now().UTC().Truncate(time.Millisecond).Add(10*time.Second)) || !deadline.After(previous) {
			t.Fatalf("created=%v savedAfter=%v deadline=%v previous=%v", item.Created, objects.savedAfter, deadline, previous)
		}
	}
	before := time.Now().UTC().Truncate(time.Millisecond)
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/remove"}, nil); c != 202 {
		t.Fatal(c)
	}
	deadline := fixtureProjectsGet(t, f, p.ID).NextGenerateAt
	if deadline.Before(before.Add(10*time.Second)) || deadline.After(time.Now().UTC().Truncate(time.Millisecond).Add(10*time.Second)) {
		t.Fatal(deadline)
	}
	f.s.Blobs = objects.Store
	f.s.storage.objects = f.s.Blobs
	restartManagedFixture(t, f)
	if got := fixtureProjectsGet(t, f, p.ID).NextGenerateAt; !got.Equal(deadline) {
		t.Fatalf("deadline not persisted: %v", got)
	}
}

func TestRejectedPutKeepsDeadline(t *testing.T) {
	t.Parallel()
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprint(deleting), func(t *testing.T) {
			f, token, p, _ := managedFixture(t)
			f.s.Config.GenerateDebounce = 10 * time.Second
			putFile(t, f, token, p.ID, "a.md", "Body")
			before := fixtureInboxList(t, f, p.ID)
			deadline := fixtureProjectsGet(t, f, p.ID).NextGenerateAt
			if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				p := fixtureProjectsGet(t, st, p.ID)
				p.Deleting, p.Revision = deleting, "large"
				fixtureProjectsPut(t, st, p.ID, p)
				fixtureOversizedWiki(t, f, st, p.ID, "large")
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
			after := fixtureInboxList(t, f, p.ID)
			if !fixtureProjectsGet(t, f, p.ID).NextGenerateAt.Equal(deadline) || len(after) != len(before) {
				t.Fatal("rejected put changed deadline or inbox")
			}
		})
	}
}

func TestDebounceChangeAppliesFromNextPut(t *testing.T) {
	t.Parallel()
	f, token, p, _ := managedFixture(t)
	f.s.Config.GenerateDebounce = 10 * time.Second
	putFile(t, f, token, p.ID, "a.md", "Body")
	old := fixtureProjectsGet(t, f, p.ID).NextGenerateAt
	// An expired stored deadline survives restarting with a different setting.
	old = old.Add(-time.Minute)
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		p := fixtureProjectsGet(t, st, p.ID)
		p.NextGenerateAt = old
		fixtureProjectsPut(t, st, p.ID, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.s.Config.GenerateDebounce = 30 * time.Second
	restartManagedFixture(t, f)
	if got := fixtureProjectsGet(t, f, p.ID).NextGenerateAt; !got.Equal(old) {
		t.Fatal(got)
	}
	before := time.Now().UTC().Truncate(time.Millisecond)
	putFile(t, f, token, p.ID, "b.md", "Body")
	got := fixtureProjectsGet(t, f, p.ID).NextGenerateAt
	if got.Before(before.Add(30*time.Second)) || got.After(time.Now().UTC().Truncate(time.Millisecond).Add(30*time.Second)) {
		t.Fatal(got)
	}
}

func TestDeadlineQueuesOnceAndClears(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	f.s.Config.GenerateDebounce = 10 * time.Second
	putFile(t, f, token, p.ID, "a.md", "Body")
	deadline := fixtureProjectsGet(t, f, p.ID).NextGenerateAt
	generationTick(t, f, deadline.Add(-time.Millisecond))
	if st := f; len(fixtureJobsList(t, st, p.ID)) != 0 || !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.Equal(deadline) {
		t.Fatal("early tick changed state")
	}
	generationTick(t, f, deadline)
	for _, at := range []time.Time{deadline, deadline.Add(24 * time.Hour)} {
		generationTick(t, f, at)
	}
	st := f
	if !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.IsZero() || len(fixtureJobsList(t, st, p.ID)) != 1 {
		t.Fatal(fixtureProjectsGet(t, st, p.ID), fixtureJobsList(t, st, p.ID))
	}
	for _, j := range fixtureJobsList(t, st, p.ID) {
		if !j.Automatic || j.Status != "queued" || j.WikiStatus != "pending" || p.Members[j.UserID] != "owner" || !j.Created.Equal(deadline) {
			t.Fatal(j)
		}
		job := startTestGeneration(t, f, j.ID)
		sendOutput(t, f, ma, job, "Scheduled Wiki")
		ma.SetStatus(job.Run.Ref.ID, "idle", "end_turn", 1)
		job = reconcileManaged(t, f, job)
		if job.Status != "completed" || job.WikiStatus != "completed" || len(job.Views) != 1 ||
			job.Views[0].Slug != "project-map" || job.Views[0].Status != "completed" {
			t.Fatalf("scheduled job did not publish its View: %+v", job)
		}
	}
}

func TestDeadlineWaitsForJob(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"queued", "running"} {
		t.Run(status, func(t *testing.T) {
			f, token, p, _ := managedFixture(t)
			f.s.Config.GenerateDebounce = 10 * time.Second
			var job fixtureJob
			if c := f.request("POST", "/api/projects/"+p.ID+"/generate", token, nil, &job); c != 202 {
				t.Fatal(c)
			}
			if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				j := fixtureJobsGet(t, st, job.ID)
				j.Status = status
				if status == "running" {
					j.Run = &fixtureAgentRun{Phase: model.PhasePreparing, PreparedAt: time.Now(), Deadline: time.Now().Add(time.Hour)}
				}
				fixtureJobsPut(t, st, j.ID, j)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			putFile(t, f, token, p.ID, "a.md", "Body")
			deadline := fixtureProjectsGet(t, f, p.ID).NextGenerateAt
			select {
			case <-f.s.wake: // Consume the manual generation's notification.
			default:
				t.Fatal("manual generation did not notify worker")
			}
			generationTick(t, f, deadline)
			if st := f; !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.Equal(deadline) || len(fixtureJobsList(t, st, p.ID)) != 1 {
				t.Fatal("job did not preserve deadline")
			}
			if len(f.s.wake) != 0 {
				t.Fatal("waiting deadline woke worker without queueing a job")
			}
			putFile(t, f, token, p.ID, "b.md", "Body")
			extended := fixtureProjectsGet(t, f, p.ID).NextGenerateAt
			if !extended.After(deadline) {
				t.Fatal("put did not extend waiting deadline")
			}
			if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				j := fixtureJobsGet(t, st, job.ID)
				j.Status = "completed"
				fixtureJobsPut(t, st, j.ID, j)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			generationTick(t, f, deadline)
			if st := f; len(fixtureJobsList(t, st, p.ID)) != 1 {
				t.Fatal("queued before extended deadline")
			}
			generationTick(t, f, extended)
			if st := f; !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.IsZero() || len(fixtureJobsList(t, st, p.ID)) != 2 {
				t.Fatal(fixtureJobsList(t, st, p.ID))
			}
		})
	}
}

func TestFailedAutomaticJobIsNotRequeued(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	f.s.Config.GenerateDebounce = 10 * time.Second
	first := putFile(t, f, token, p.ID, "a.md", "Body")
	generationTick(t, f, fixtureProjectsGet(t, f, p.ID).NextGenerateAt)
	var job fixtureJob
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		for _, j := range fixtureJobsList(t, st, p.ID) {
			job = j
			job.Status = "queued"
			fixtureJobsPut(t, st, job.ID, job)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !job.Automatic {
		t.Fatal(job)
	}
	if err := f.s.startAgentJob(t.Context(), fixtureRow(job)); err != nil {
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
	st := f
	if len(fixtureJobsList(t, st, p.ID)) != 1 || fixtureInboxGet(t, st, first.ID).ID == "" || fixtureInboxGet(t, st, first.ID).RunID != "" || !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.IsZero() {
		t.Fatal("failed job was retried or did not release inbox")
	}
	putFile(t, f, token, p.ID, "b.md", "Body")
	generationTick(t, f, fixtureProjectsGet(t, f, p.ID).NextGenerateAt)
	if st := f; len(fixtureJobsList(t, st, p.ID)) != 2 {
		t.Fatal("new put did not schedule generation")
	}
}

func TestZeroDebounceQueuesOnNextTick(t *testing.T) {
	t.Parallel()
	for _, debounce := range []time.Duration{0, time.Nanosecond} {
		t.Run(debounce.String(), func(t *testing.T) {
			f, token, p, _ := managedFixture(t)
			f.s.Config.GenerateDebounce = debounce
			before := time.Now().UTC().Truncate(time.Millisecond)
			putFile(t, f, token, p.ID, "a.md", "Body")
			st := f
			if len(fixtureJobsList(t, st, p.ID)) != 0 || fixtureProjectsGet(t, st, p.ID).NextGenerateAt.Before(before) {
				t.Fatal("put must only set the deadline")
			}
			generationTick(t, f, time.Now().UTC().Truncate(time.Millisecond))
			st = f
			if len(fixtureJobsList(t, st, p.ID)) != 1 || !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.IsZero() {
				t.Fatal(fixtureJobsList(t, st, p.ID))
			}
		})
	}
}

func TestStoredFixedDeadlineFiresOnce(t *testing.T) {
	t.Parallel()
	f, token, p, _ := managedFixture(t)
	putFile(t, f, token, p.ID, "a.md", "Body")
	deadline := time.Now().UTC().Truncate(time.Millisecond).Add(time.Hour)
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		p := fixtureProjectsGet(t, st, p.ID)
		p.NextGenerateAt = deadline
		fixtureProjectsPut(t, st, p.ID, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.s.Config.GenerateDebounce = 10 * time.Second
	restartManagedFixture(t, f)
	generationTick(t, f, deadline.Add(-time.Millisecond))
	if st := f; len(fixtureJobsList(t, st, p.ID)) != 0 || !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.Equal(deadline) {
		t.Fatal("stored deadline changed")
	}
	generationTick(t, f, deadline)
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		for id, j := range fixtureJobsList(t, st, p.ID) {
			j.Status = "completed"
			fixtureJobsPut(t, st, id, j)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	generationTick(t, f, deadline.Add(48*time.Hour))
	if st := f; len(fixtureJobsList(t, st, p.ID)) != 1 || !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.IsZero() {
		t.Fatal("fixed deadline repeated")
	}
}

func TestFullInboxLetsDeadlinePass(t *testing.T) {
	t.Parallel()
	f, token, p, _ := managedFixture(t)
	f.s.Config.GenerateDebounce = 10 * time.Second
	putFile(t, f, token, p.ID, "a.md", "Body")
	deadline := fixtureProjectsGet(t, f, p.ID).NextGenerateAt
	if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
		for i := 1; i < maxInboxItems; i++ {
			id := fmt.Sprintf("item%d", i)
			fixtureInboxPut(t, st, id, model.InboxItem{ID: id, ProjectID: p.ID, SourceURL: "https://example.com/" + id, Remove: true})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/full"}, nil); c != 429 {
		t.Fatal(c)
	}
	if st := f; !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.Equal(deadline) || len(fixtureInboxList(t, st, p.ID)) != maxInboxItems {
		t.Fatal("rejected put changed state")
	}
	generationTick(t, f, deadline)
	if st := f; len(fixtureJobsList(t, st, p.ID)) != 1 || !fixtureProjectsGet(t, st, p.ID).NextGenerateAt.IsZero() {
		t.Fatal(fixtureJobsList(t, st, p.ID))
	}
}

func TestIdleTickUsesSQLAndPreservesDeadlines(t *testing.T) {
	t.Parallel()
	f, _, p, _ := managedFixture(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
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
			if err := f.stateStore.Write(context.Background(), func(st *store.Queries) error {
				p := fixtureProjectsGet(t, st, p.ID)
				p.NextGenerateAt = tc.deadline
				p.Deleting = tc.deleting
				fixtureProjectsPut(t, st, p.ID, p)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				generationTick(t, f, now)
			}
			if len(f.s.wake) != 0 {
				t.Fatal("idle tick woke worker")
			}
			if got := fixtureProjectsGet(t, f, p.ID).NextGenerateAt; !got.Equal(tc.deadline) {
				t.Fatal("idle tick changed deadline")
			}
		})
	}
}
