package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestInboxAcceptanceReplacementAndLimits(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	base := "/api/projects/" + p.Namespace
	for _, tc := range []struct {
		req    model.PutRequest
		remove bool
	}{
		{model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://example.com/a", "Source body")}}, false},
		{model.PutRequest{File: &model.FileInput{Name: "a.md", Content: testSourceContent("https://example.com/a", "---\nremove: true\n---\nBody")}}, false},
		{model.PutRequest{Remove: true, SourceURL: "https://example.com/a"}, true},
	} {
		var result map[string]any
		if code := f.request("POST", base+"/put", token, tc.req, &result); code != 202 || result == nil || len(result) != 0 {
			t.Fatal(code, result)
		}
		if item := waitingInboxItem(t, f, p.ID, "https://example.com/a"); item.Remove != tc.remove {
			t.Fatal("stored remove flag", item)
		}
		f.db.View(func(st model.State) error {
			if len(st.Inbox) != 1 || len(st.Jobs) != 0 {
				t.Fatal("not replaced", st.Inbox)
			}
			return nil
		})
	}
	f.db.Update(func(st *model.State) error {
		for id, v := range st.Inbox {
			v.RunID = "running"
			st.Inbox[id] = v
		}
		return nil
	})
	if c := f.request("POST", base+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://example.com/a", "Source body")}}, nil); c != 202 {
		t.Fatal(c)
	}
	var projects []model.ProjectEntry
	if c := f.request("GET", "/api/projects", token, nil, &projects); c != 200 || len(projects) != 1 || projects[0].InboxCount != 2 || len(projects[0].Inbox) != 2 {
		t.Fatal(c, projects)
	}
	f.db.Update(func(st *model.State) error {
		for i := len(st.Inbox); i < 200; i++ {
			id := fmt.Sprint(i)
			st.Inbox[id] = model.InboxItem{ID: id, ProjectID: p.ID, SourceURL: "https://example.com/" + id}
		}
		return nil
	})
	if c := f.request("POST", base+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://example.com/new", "Source body")}}, nil); c != 429 {
		t.Fatal(c)
	}
	if c := f.request("POST", base+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/a"}, nil); c != 202 {
		t.Fatal(c)
	}
	f.db.Update(func(st *model.State) error {
		q := st.Projects[p.ID]
		q.Revision = "r"
		st.Projects[p.ID] = q
		st.Revisions["r"] = model.Revision{ID: "r", ProjectID: p.ID, StoredBytes: f.s.Config.MaxWikiBytes + 1}
		return nil
	})
	if c := f.request("POST", base+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://example.com/a", "Source body")}}, nil); c != 413 {
		t.Fatal(c)
	}
	if c := f.request("POST", base+"/put", token, model.PutRequest{Remove: true, SourceURL: "https://example.com/a"}, nil); c != 202 {
		t.Fatal(c)
	}
}

func TestInboxRejectsInvalidPutWithoutState(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	for _, req := range []any{
		map[string]any{"url": "https://example.com/a"},
		model.PutRequest{File: &model.FileInput{Name: "a.md", Content: testSourceContent("https://example.com/a", "body")}, Remove: true},
		map[string]any{"url": "https://example.com/a", "remove": true, "source_url": "https://example.com/a"},
		map[string]any{"files": []model.FileInput{{Name: "a.md"}, {Name: "b.md"}}},
		model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("http://example.com/a", "Source body")}}, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://127.0.0.1/a", "Source body")}},
		model.PutRequest{File: &model.FileInput{Name: "a.md", Content: "body"}},
	} {
		if c := f.request("POST", "/api/projects/"+p.Namespace+"/put", token, req, nil); c != 400 {
			t.Fatal(c, req)
		}
	}
	if c := f.request("POST", "/api/projects/"+p.Namespace+"/put", token, model.PutRequest{File: &model.FileInput{Name: "a.md", Content: testSourceContent("https://example.com/a", strings.Repeat("a", 1<<20))}}, nil); c != 413 {
		t.Fatal(c)
	}
	f.db.View(func(st model.State) error {
		if len(st.Inbox) != 0 {
			t.Fatal(st.Inbox)
		}
		return nil
	})
}

func TestInboxOrphanGC(t *testing.T) {
	f := setupWithWorker(t, false)
	token := f.login("owner@example.com")
	p := securityProject(t, f, token)
	item := model.InboxItem{ID: "orphan", ProjectID: p.ID}
	if err := f.s.Blobs.Put(t.Context(), item.BodyKey(), []byte("body")); err != nil {
		t.Fatal(err)
	}
	n, err := f.s.gc(httptest.NewRequest("POST", "/", nil), p.ID)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestClientVersionGate(t *testing.T) {
	for _, tc := range []struct {
		v, path, auth string
		want          int
	}{
		{"", "/api/projects", "Bearer old", 426}, {"0.0.1", "/api/projects", "Bearer old", 426},
		{"garbage", "/api/projects", "Bearer old", 426}, {"0.1.0", "/api/projects", "Bearer new", 0},
		{"dev", "/api/projects", "Bearer dev", 0}, {"", "/mcp", "Bearer connector", 0}, {"", "/api/projects", "", 0},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("Authorization", tc.auth)
		r.Header.Set("Kioku-Client-Version", tc.v)
		err := checkClientVersion(r)
		if tc.want == 0 {
			if err != nil {
				t.Fatal(err)
			}
		} else if e, ok := err.(apiError); !ok || e.status != tc.want {
			t.Fatal(err)
		}
	}
}

func TestConnectorAndAgentInboxPuts(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	_, agent := testAgent(t, f, human, p.ID, "write")
	if c := f.request("POST", "/api/projects/"+p.Namespace+"/put", agent, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://example.com/agent", "Source body")}}, nil); c != 202 {
		t.Fatal(c)
	}
	access := connectorAccessToken(t, f, human, p.ID)
	for _, args := range []map[string]any{
		{"project": p.Namespace, "name": "source.md", "content": testSourceContent("https://example.com/mcp", "MCP source body")},
		{"project": p.Namespace, "remove": true, "source_url": "https://example.com/mcp"},
	} {
		var result map[string]any
		if msg := connectorTool(t, f, access, "source_put", args, &result); msg != "" || result == nil || len(result) != 0 {
			t.Fatal(msg, result)
		}
	}
	// No version header must be rejected by the real HTTP middleware.
	b, _ := json.Marshal(model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent("https://example.com/old", "Source body")}})
	req, _ := http.NewRequest("POST", f.url+"/api/projects/"+p.Namespace+"/put", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+human)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 426 {
		t.Fatal(resp.StatusCode)
	}
}
