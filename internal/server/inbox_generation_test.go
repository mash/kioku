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
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{URL: "https://example.com/private"}, nil); c != 202 {
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
	var replacement model.InboxItem
	if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{Remove: true, SourceURL: url}, &replacement); c != 202 {
		t.Fatal(c)
	}
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
	f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{URL: "https://example.com/pending"}, nil)
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
			if st.Inbox[id].Status != "removed" {
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
