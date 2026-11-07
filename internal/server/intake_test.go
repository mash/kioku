package server

import (
	"testing"

	"github.com/mash/kioku/internal/model"
)

func putFile(t *testing.T, f *fixture, token, pid, name, body string) model.Job {
	t.Helper()
	var j model.Job
	if c := f.seedSource("POST", "/api/projects/"+pid+"/put", token, model.PutRequest{File: &model.FileInput{Name: name, Content: body, SourceURL: testSourceURL(name, body)}}, &j); c != 202 {
		t.Fatal(c)
	}
	if j.Kind != "put" || j.Status != "completed" || j.Revision != "" {
		t.Fatal("unexpected source receipt", j)
	}
	return j
}
