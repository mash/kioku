package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
)

func TestQuotaRetentionDeletionAuditAndMetrics(t *testing.T) {
	f := setup(t)
	token := f.login("owner@example.com")
	f.s.Config.MaxSources = 1
	f.s.Config.MetricsToken = "metrics-secret"
	var p model.Project
	f.request("POST", "/api/projects", token, map[string]string{"name": "Operations"}, &p)
	base := "/api/projects/" + p.ID
	var job model.Job
	input := model.PutRequest{Files: []model.FileInput{{Name: "one.md", Content: "One source", SourceURL: testSourceURL("one.md", "One source")}}}
	if c := f.request("POST", base+"/put", token, input, &job); c != 202 {
		t.Fatal(c)
	}
	f.wait(p.ID, token, f.generate(p.ID, token, false).ID)
	var second model.Job
	f.request("POST", base+"/put", token, input, &second)
	f.wait(p.ID, token, f.generate(p.ID, token, true).ID)
	input.Files[0].Name = "two.md"
	if c := f.request("POST", base+"/put", token, input, nil); c != 413 {
		t.Fatalf("quota: %d", c)
	}
	if err := f.s.Blobs.Put(context.Background(), blob.Key("projects/"+p.ID+"/orphan"), []byte("orphan")); err != nil {
		t.Fatal(err)
	}
	if c := f.request("POST", base+"/maintenance", token, map[string]int{"keep": 1}, nil); c != 200 {
		t.Fatal(c)
	}
	if _, err := f.s.Blobs.Get(context.Background(), blob.Key("projects/"+p.ID+"/orphan")); err == nil {
		t.Fatal("orphan retained")
	}
	var revisions int
	_ = f.db.View(func(st model.State) error {
		for _, r := range st.Revisions {
			if r.ProjectID == p.ID {
				revisions++
			}
		}
		return nil
	})
	if revisions != 1 {
		t.Fatal("retention", revisions)
	}
	var audits []model.Audit
	if c := f.request("GET", base+"/audit", token, nil, &audits); c != 200 || len(audits) < 3 {
		t.Fatal("missing audit", c, len(audits))
	}
	if c := f.request("GET", "/metrics", token, nil, nil); c != 401 {
		t.Fatal("metrics not protected")
	}
	if c := f.request("GET", "/metrics", "metrics-secret", nil, nil); c != 200 {
		t.Fatal(c)
	}
	var deletion model.Job
	if c := f.request("DELETE", base+"/sources/"+job.SourceIDs[0], token, nil, &deletion); c != 202 {
		t.Fatal(c)
	}
	f.wait(p.ID, token, deletion.ID)
	var afterDelete model.Snapshot
	if c := f.request("GET", base+"/snapshot", token, nil, &afterDelete); c != 200 || strings.Contains(afterDelete.Files["wiki/inbox.md"], "One source") {
		t.Fatal("deleted source still exposed", c)
	}
	var regenerate model.Job
	f.request("POST", base+"/generate", token, nil, &regenerate)
	f.wait(p.ID, token, regenerate.ID)
	var snap model.Snapshot
	f.request("GET", base+"/snapshot", token, nil, &snap)
	for _, v := range snap.Files {
		if strings.Contains(v, "One source") {
			t.Fatal("deleted source retained in wiki")
		}
	}
	if c := f.request("DELETE", base, token, nil, nil); c != 200 {
		t.Fatal(c)
	}
	if c := f.request("GET", base+"/snapshot", token, nil, nil); c != 404 {
		t.Fatal(c)
	}
	keys, err := f.s.Blobs.List(context.Background(), "projects/"+p.ID+"/")
	if err != nil || len(keys) > 0 {
		t.Fatal("project objects not deleted", keys, err)
	}
}
func TestRateLimit(t *testing.T) {
	f := setup(t)
	f.s.Config.RatePerMinute = 2
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("GET", f.url+"/healthz", nil)
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if i == 2 && resp.StatusCode != 429 {
			t.Fatal("rate limit missing", resp.StatusCode)
		}
	}
}
func TestRenderedMarkdownAndGraphAreSafe(t *testing.T) {
	body := markdownView("# Heading\n[Link](topic.md)\n<script>alert(1)</script>\n[bad](javascript:alert(2))", "wiki/index.md", []string{"wiki/topic.md"})
	if strings.Contains(string(body), "<script>") || strings.Contains(string(body), "javascript:") || !strings.Contains(string(body), "?file=wiki%2Ftopic.md") {
		t.Fatal(body)
	}
	graph := graphView(`{"nodes":[{"id":"wiki/index.md","title":"<script>alert(1)</script>"}],"edges":[]}`, "")
	if strings.Contains(string(graph), "<script>") || !strings.Contains(string(graph), "<svg") {
		t.Fatal(graph)
	}
}
