package server

import (
	"context"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func putFile(t *testing.T, f *fixture, token, pid, name, body string) model.InboxItem {
	t.Helper()
	url := testSourceURL(name, body)
	var result map[string]any
	if c := f.request("POST", "/api/projects/"+pid+"/put", token, model.PutRequest{File: &model.FileInput{Name: name, Content: testSourceContent(url, body)}}, &result); c != 202 || result["source_url"] != url || len(result) != 1 {
		t.Fatal(c, result)
	}
	j := waitingInboxItem(t, f, pid, url)
	if j.Remove {
		t.Fatal("unexpected source receipt", j)
	}
	return j
}

func waitingInboxItem(t *testing.T, f *fixture, pid, url string) model.InboxItem {
	t.Helper()
	var result model.InboxItem
	if err := f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		for _, item := range fixtureInboxList(t, st, pid) {
			if item.ProjectID == pid && item.SourceURL == url && item.RunID == "" {
				if result.ID != "" {
					t.Fatal("duplicate waiting inbox items", pid, url)
				}
				result = item
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result.ID == "" {
		t.Fatal("waiting inbox item not found", pid, url)
	}
	return result
}
