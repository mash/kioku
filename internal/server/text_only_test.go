package server

import (
	"context"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func TestTextOnlySourceStorage(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	base := "/api/projects/" + p.ID
	original := "<html><head><script>never persist this script</script></head><body><h1>Evidence</h1><p>Readable text</p></body></html>"
	input := model.FileInput{Name: "evidence.html", Content: original, SourceURL: "https://example.com/evidence"}
	var job model.Job
	if code := f.request("POST", base+"/put", token, model.PutRequest{Files: []model.FileInput{input}}, &job); code != 202 {
		t.Fatal(code)
	}
	var src model.Source
	if err := f.db.View(func(st model.State) error { src = st.Sources[job.SourceIDs[0]]; return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(src.TextKey), "/extracted.md") || src.URL != input.SourceURL || src.ContentHash != auth.Hash(original) {
		t.Fatalf("metadata: %+v", src)
	}
	text, err := f.s.Blobs.Get(context.Background(), src.TextKey)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "Readable text") || strings.Contains(string(text), "script") || strings.Contains(string(text), "<html>") {
		t.Fatalf("stored text: %s", text)
	}
	keys, err := f.s.Blobs.List(context.Background(), "projects/"+p.ID+"/sources/")
	if err != nil || len(keys) != 1 || keys[0] != src.TextKey {
		t.Fatal("stored objects", keys, err)
	}
	if src.StoredBytes != int64(len(text)) || src.Bytes != int64(len(text)+len(src.Title)+len(src.URL)) {
		t.Fatal("quota", src)
	}
	if code := f.request("GET", base+"/sources/"+src.ID+"/original", token, nil, nil); code != 404 {
		t.Fatal("original download", code)
	}
	// The source receipt remains idempotent without retaining the original body.
	var retry model.Job
	if code := f.request("POST", base+"/put", token, model.PutRequest{Files: []model.FileInput{input}}, &retry); code != 202 || retry.SourceIDs[0] != src.ID {
		t.Fatal("retry", code, retry)
	}
}

func TestMissingSourceURLRejectsEntireBatch(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	input := model.PutRequest{Files: []model.FileInput{
		{Name: "valid.md", Content: "Evidence", SourceURL: "https://example.com/valid"},
		{Name: "missing.md", Content: "Evidence"},
	}}
	if code := f.request("POST", "/api/projects/"+p.ID+"/put", token, input, nil); code != 400 {
		t.Fatal(code)
	}
	if err := f.db.View(func(st model.State) error {
		if len(st.Sources) != 0 || len(st.Uploads) != 0 {
			t.Fatal("partially stored batch")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	keys, err := f.s.Blobs.List(context.Background(), "projects/"+p.ID+"/sources/")
	if err != nil || len(keys) != 0 {
		t.Fatal(keys, err)
	}
}

// A provenance URL must not collapse separate file records into URL revisions.
func TestFilesSharingSourceURLRemainGenerationInputs(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	base := "/api/projects/" + p.ID
	var put model.Job
	input := model.PutRequest{Files: []model.FileInput{
		{Name: "first.md", Content: "First observation", SourceURL: "https://example.com/shared"},
		{Name: "second.md", Content: "Second observation", SourceURL: "https://example.com/shared"},
	}}
	if code := f.request("POST", base+"/put", token, input, &put); code != 202 {
		t.Fatal(code)
	}
	job := f.generate(p.ID, token, false)
	if err := f.db.Update(func(st *model.State) error {
		j := st.Jobs[job.ID]
		j.Status = "running"
		st.Jobs[job.ID] = j
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	in, err := f.s.agentSnapshot(job)
	if err != nil || len(in.sources) != 2 {
		t.Fatal("managed generation inputs", len(in.sources), err)
	}
	if _, err := f.s.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var snap model.Snapshot
	if code := f.request("GET", base+"/snapshot", token, nil, &snap); code != 200 {
		t.Fatal(code)
	}
	for _, id := range put.SourceIDs {
		if snap.Files["wiki/source-"+id+".md"] == "" {
			t.Fatal("file excluded from wiki", id)
		}
	}
}
