package server

import (
	"testing"

	"github.com/mash/kioku/internal/model"
)

func putFile(t *testing.T, f *fixture, token, pid, name, body string) model.InboxItem {
	t.Helper()
	url := testSourceURL(name, body)
	var result map[string]any
	if c := f.request("POST", "/api/projects/"+pid+"/put", token, model.PutRequest{File: &model.FileInput{Name: name, Content: testSourceContent(url, body)}}, &result); c != 202 || result == nil || len(result) != 0 {
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
	if err := f.db.View(func(st model.State) error {
		for _, item := range st.Inbox {
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
