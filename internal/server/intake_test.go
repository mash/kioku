package server

import (
	"testing"

	"github.com/mash/kioku/internal/model"
)

func putFile(t *testing.T, f *fixture, token, pid, name, body string) model.InboxItem {
	t.Helper()
	var j model.InboxItem
	if c := f.request("POST", "/api/projects/"+pid+"/put", token, model.PutRequest{File: &model.FileInput{Name: name, Content: testSourceContent(testSourceURL(name, body), body)}}, &j); c != 202 {
		t.Fatal(c)
	}
	if j.Status != "fetched" {
		t.Fatal("unexpected source receipt", j)
	}
	return j
}
