package server

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func connectorTool(t *testing.T, f *fixture, token, name string, args, value any) string {
	t.Helper()
	params := map[string]any{"name": name, "_meta": map[string]any{"progressToken": "test"}}
	if args != nil {
		params["arguments"] = args
	}
	raw := connectorRPC(t, f, token, "tools/call", params)
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatal("invalid tool result", err, string(raw))
	}
	if result.IsError {
		return result.Content[0].Text
	}
	if value != nil {
		if err := json.Unmarshal([]byte(result.Content[0].Text), value); err != nil {
			t.Fatal("invalid tool JSON", err)
		}
	}
	return ""
}

type connectorReadResult struct {
	Project    string `json:"project"`
	Path       string `json:"path"`
	FileBytes  int    `json:"file_bytes"`
	Offset     int    `json:"offset"`
	NextOffset *int   `json:"next_offset"`
	Content    string `json:"content"`
}

type connectorSearchResult struct {
	Project string `json:"project"`
	Query   string `json:"query"`
	Results []struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	} `json:"results"`
	Truncated bool `json:"truncated"`
}

func TestConnectorWikiReadAndSearch(t *testing.T) {
	f := setupWithWorker(t, true)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	access := connectorAccessToken(t, f, human, project.ID)
	args := map[string]any{"project": project.Namespace}
	if msg := connectorTool(t, f, access, "wiki_read", args, nil); msg != "project has no wiki yet" {
		t.Fatal("empty project", msg)
	}
	var search connectorSearchResult
	if msg := connectorTool(t, f, access, "wiki_search", map[string]any{"project": project.Namespace, "query": "absent"}, &search); msg != "" || search.Results == nil || len(search.Results) != 0 || search.Truncated {
		t.Fatal("empty search", msg, search)
	}
	content := "MixedCaseNeedle\n" + strings.Repeat("界", 12000) + "\n" + strings.Repeat("FloodNeedle\n", 55) + "ẞ" + strings.Repeat("x", 300) + "DeepNeedle" + "Ⱥ" + strings.Repeat("y", 300)
	var put model.Job
	base := "/api/projects/" + project.ID
	if code := f.request("POST", base+"/put", human, model.PutRequest{File: &model.FileInput{Name: "evidence.md", Content: content, SourceURL: testSourceURL("evidence.md", content)}}, &put); code != 202 {
		t.Fatal("put source", code)
	}
	f.wait(project.ID, human, f.generate(project.ID, human, false).ID)
	var snapshot model.Snapshot
	if code := f.request("GET", base+"/snapshot", human, nil, &snapshot); code != 200 {
		t.Fatal("snapshot", code)
	}
	var projects struct {
		Projects []struct{ Namespace, Name, Role string } `json:"projects"`
	}
	if msg := connectorTool(t, f, access, "projects_list", nil, &projects); msg != "" || len(projects.Projects) != 1 {
		t.Fatal("project list", msg, projects)
	}
	if p := projects.Projects[0]; p.Namespace != project.Namespace || p.Name != project.Name || p.Role != "owner" {
		t.Fatal("project metadata", p)
	}
	var index connectorReadResult
	if msg := connectorTool(t, f, access, "wiki_read", args, &index); msg != "" || index.Project != project.Namespace || index.Path != "wiki/index.md" || index.Content != snapshot.Files[index.Path] || index.FileBytes != len(index.Content) || index.Offset != 0 || index.NextOffset != nil {
		t.Fatal("index read", msg, index)
	}
	name := sourcePage(put.ID)
	args["path"] = name
	var first, second connectorReadResult
	if msg := connectorTool(t, f, access, "wiki_read", args, &first); msg != "" || first.NextOffset == nil || len(first.Content) > 32768 || !utf8.ValidString(first.Content) || first.Offset != 0 {
		t.Fatal("first chunk", msg)
	}
	if *first.NextOffset != len(first.Content) || first.FileBytes != len(snapshot.Files[name]) {
		t.Fatal("chunk metadata", first.NextOffset, first.FileBytes)
	}
	args["offset"] = *first.NextOffset
	if msg := connectorTool(t, f, access, "wiki_read", args, &second); msg != "" || second.NextOffset != nil || second.Offset != *first.NextOffset || second.FileBytes != first.FileBytes || len(second.Content) > 32768 || !utf8.ValidString(second.Content) || first.Content+second.Content != snapshot.Files[name] {
		t.Fatal("second chunk", msg)
	}
	args["offset"] = first.FileBytes
	var end connectorReadResult
	if msg := connectorTool(t, f, access, "wiki_read", args, &end); msg != "" || end.Content != "" || end.NextOffset != nil {
		t.Fatal("end offset", msg, end)
	}
	for _, offset := range []int{-1, first.FileBytes + 1, *first.NextOffset + 1} {
		args["offset"] = offset
		if msg := connectorTool(t, f, access, "wiki_read", args, nil); msg != "invalid offset" {
			t.Fatal("invalid offset accepted", offset, msg)
		}
	}
	for _, tc := range []struct{ path, message string }{
		{"../x.md", "invalid wiki path"},
		{"sources/x", "invalid wiki path"},
		{"wiki/missing.md", "wiki file not found; read maps/graph.json for the page list"},
	} {
		if msg := connectorTool(t, f, access, "wiki_read", map[string]any{"project": project.Namespace, "path": tc.path}, nil); msg != tc.message {
			t.Fatal("path error", tc.path, msg)
		}
	}
	searchArgs := map[string]any{"project": project.Namespace, "query": "  mixedcaseneedle  "}
	if msg := connectorTool(t, f, access, "wiki_search", searchArgs, &search); msg != "" || search.Project != project.Namespace || search.Query != "mixedcaseneedle" || len(search.Results) != 1 || search.Truncated {
		t.Fatal("mixed case search", msg, search)
	}
	match := search.Results[0]
	if match.Path != name || match.Text != "MixedCaseNeedle" || strings.Split(snapshot.Files[name], "\n")[match.Line-1] != match.Text {
		t.Fatal("search location", match)
	}
	searchArgs["query"] = "floodneedle"
	if msg := connectorTool(t, f, access, "wiki_search", searchArgs, &search); msg != "" || len(search.Results) != 50 || !search.Truncated {
		t.Fatal("search cap", msg, search)
	}
	for i, match := range search.Results {
		if match.Path != name || match.Text != "FloodNeedle" || (i > 0 && match.Line != search.Results[i-1].Line+1) {
			t.Fatal("search order", match)
		}
	}
	searchArgs["query"] = "界"
	if msg := connectorTool(t, f, access, "wiki_search", searchArgs, &search); msg != "" || len(search.Results) != 1 || len(search.Results[0].Text) > 240 || !utf8.ValidString(search.Results[0].Text) {
		t.Fatal("search text cap", msg, search)
	}
	searchArgs["query"] = "deepneedle"
	if msg := connectorTool(t, f, access, "wiki_search", searchArgs, &search); msg != "" || len(search.Results) != 1 || len(search.Results[0].Text) > 240 || strings.Index(search.Results[0].Text, "DeepNeedle") != 120 {
		t.Fatal("search window", msg, search)
	}
}

func TestConnectorWikiArgumentsAndMembership(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	access := connectorAccessToken(t, f, human, project.ID)
	outsider := f.login("outsider@example.com")
	outsiderProject := securityProject(t, f, outsider)
	otherAccess := connectorAccessToken(t, f, outsider, outsiderProject.ID)
	for _, name := range []string{"wiki_read", "wiki_search", "source_put"} {
		for _, namespace := range []string{project.Namespace, "unknown/project"} {
			args := map[string]any{"project": namespace}
			if name == "wiki_search" {
				args["query"] = "test"
			}
			if name == "source_put" {
				args["name"] = "record.md"
				args["content"] = "text"
			}
			if msg := connectorTool(t, f, otherAccess, name, args, nil); msg != "project not found" {
				t.Fatal("project disclosure", name, namespace, msg)
			}
		}
	}
	var projects struct {
		Projects []struct{ Namespace string } `json:"projects"`
	}
	if msg := connectorTool(t, f, otherAccess, "projects_list", nil, &projects); msg != "" || len(projects.Projects) != 1 || projects.Projects[0].Namespace != outsiderProject.Namespace {
		t.Fatal("project list disclosure", msg, projects)
	}
	if msg := connectorTool(t, f, otherAccess, "projects_list", json.RawMessage(`null`), &projects); msg != "" {
		t.Fatal("null arguments rejected", msg)
	}
	for _, tc := range []struct {
		name    string
		args    any
		message string
	}{
		{"wiki_read", nil, "unknown tool or invalid arguments"},
		{"wiki_read", map[string]any{"project": 1}, "unknown tool or invalid arguments"},
		{"wiki_read", map[string]any{"Project": project.Namespace}, "unknown tool or invalid arguments"},
		{"wiki_read", map[string]any{"project": project.Namespace, "PATH": "wiki/index.md"}, "unknown tool or invalid arguments"},
		{"wiki_read", map[string]any{"project": project.Namespace, "offset": 1.5}, "unknown tool or invalid arguments"},
		{"wiki_read", map[string]any{"project": project.Namespace, "path": ""}, "invalid wiki path"},
		{"wiki_read", map[string]any{"project": ""}, "project not found"},
		{"wiki_search", map[string]any{"project": project.Namespace}, "unknown tool or invalid arguments"},
		{"wiki_search", map[string]any{"project": project.Namespace, "query": "  "}, "query must be 1-200 bytes"},
		{"wiki_search", map[string]any{"project": project.Namespace, "query": strings.Repeat("界", 67)}, "query must be 1-200 bytes"},
		{"projects_list", map[string]any{"project": project.Namespace}, "unknown tool or invalid arguments"},
		{"cli_login_code", nil, "project not found"},
		{"unknown", nil, "unknown tool or invalid arguments"},
	} {
		if msg := connectorTool(t, f, access, tc.name, tc.args, nil); msg != tc.message {
			t.Fatal("argument error", tc.name, tc.args, msg)
		}
	}
	_ = f.db.Update(func(st *model.State) error {
		p := st.Projects[project.ID]
		p.Deleting = true
		st.Projects[p.ID] = p
		return nil
	})
	if msg := connectorTool(t, f, access, "projects_list", nil, &projects); msg != "" || len(projects.Projects) != 0 {
		t.Fatal("deleting project listed", msg, projects)
	}
}

func TestConnectorSourcePutForOwnerAndMember(t *testing.T) {
	for _, role := range []string{"owner", "member"} {
		t.Run(role, func(t *testing.T) {
			f := setupWithWorker(t, true)
			human := f.login("owner@example.com")
			project := securityProject(t, f, human)
			base := "/api/projects/" + project.ID
			if code := f.request("POST", base+"/put", human, model.PutRequest{File: &model.FileInput{Name: "initial.md", Content: "Initial evidence", SourceURL: testSourceURL("initial.md", "Initial evidence")}}, nil); code != 202 {
				t.Fatal("initial put", code)
			}
			f.wait(project.ID, human, f.generate(project.ID, human, false).ID)
			caller := human
			if role == "member" {
				caller = f.login("member@example.com")
				var invite struct {
					URL string `json:"url"`
				}
				if code := f.request("POST", base+"/invites", human, map[string]string{"email": "member@example.com"}, &invite); code != 201 {
					t.Fatal("invite", code)
				}
				if code := f.request("POST", "/api/join", caller, map[string]string{"token": invite.URL}, nil); code != 200 {
					t.Fatal("join", code)
				}
			}
			access := connectorAccessToken(t, f, caller, project.ID)
			var projects struct {
				Projects []struct{ Namespace, Role string } `json:"projects"`
			}
			if msg := connectorTool(t, f, access, "projects_list", nil, &projects); msg != "" || len(projects.Projects) != 1 || projects.Projects[0].Role != role || projects.Projects[0].Namespace != project.Namespace {
				t.Fatal("project role", msg, projects)
			}
			var read connectorReadResult
			if msg := connectorTool(t, f, access, "wiki_read", map[string]any{"project": project.Namespace}, &read); msg != "" || read.Content == "" {
				t.Fatal("member read", msg)
			}
			var before model.Project
			var jobsBefore int
			if err := f.db.View(func(st model.State) error {
				before = st.Projects[project.ID]
				jobsBefore = len(st.Jobs)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var result model.InboxItem
			content := "---\nsource_url: https://example.com/record\n---\nA new shared record"
			args := map[string]any{"project": project.Namespace, "name": "record.md", "content": content}
			if msg := connectorTool(t, f, access, "source_put", args, &result); msg != "" || result.ID == "" || result.Status != "fetched" {
				t.Fatal(msg, result)
			}
			var submitter string
			if err := f.db.View(func(st model.State) error {
				submitter = st.Inbox[result.ID].Submitter
				if st.Projects[project.ID].Revision != before.Revision || len(st.Jobs) != jobsBefore || submitter != st.Sessions[auth.Hash(caller)].UserID {
					t.Error("inbox acceptance changed generation or submitter")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var audit []model.Audit
			if code := f.request("GET", base+"/audit", human, nil, &audit); code != 200 {
				t.Fatal("audit", code)
			}
			count := 0
			for _, record := range audit {
				if record.Detail == "mcp source_put" {
					count++
					if record.ProjectID != project.ID || record.UserID != submitter || record.Action != "POST" {
						t.Fatal("audit metadata", record)
					}
				}
			}
			if count != 1 {
				t.Fatal("audit count", count)
			}
		})
	}
}

func TestConnectorSourcePutValidation(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	project := securityProject(t, f, human)
	access := connectorAccessToken(t, f, human, project.ID)
	for _, tc := range []struct {
		args    map[string]any
		message string
	}{
		{map[string]any{"project": project.Namespace, "name": "record.pdf", "content": "text"}, "unsupported file extension; use text, Markdown, HTML or code"},
		{map[string]any{"project": project.Namespace, "name": "record.md"}, "unknown tool or invalid arguments"},
		{map[string]any{"project": project.Namespace, "name": "record.md", "content": "Evidence"}, "original source URL is required: use source_url frontmatter or --source-url"},
		{map[string]any{"project": project.Namespace, "name": "record.md", "content": ""}, "source is empty"},
		{map[string]any{"project": project.Namespace, "name": "record.md", "content": "text", "url": "https://example.com"}, "unknown tool or invalid arguments"},
	} {
		if msg := connectorTool(t, f, access, "source_put", tc.args, nil); msg != tc.message {
			t.Fatal("source validation", msg)
		}
	}
	if err := f.db.View(func(st model.State) error {
		if len(st.Jobs) != 0 || len(st.Inbox) != 0 || len(st.Audit) != 0 {
			t.Error("rejected source changed state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
