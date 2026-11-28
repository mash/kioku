package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
	"github.com/mash/kioku/internal/store"
)

// The fake supplies model output. This verifies the archive/publication contract,
// not whether a real model follows the prompt's filename rules.
func TestSourcePagesRoundTripPreservesNamesAndSlugCollision(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	urls := []string{"https://example.com/a", "https://example.com/b"}
	putText := func(url, body string) {
		t.Helper()
		if c := f.request("POST", "/api/projects/"+p.ID+"/put", token, model.PutRequest{File: &model.FileInput{Name: "source.md", Content: testSourceContent(url, body)}}, nil); c != 202 {
			t.Fatal(c)
		}
	}
	putText(urls[0], "# Title\nFirst")
	putText(urls[1], "# Title\nSecond")
	pages := map[string]string{
		"wiki/index.md":          "# Wiki\n[Analysis](analysis.md)",
		"wiki/analysis.md":       "[One](source-title.md) and [Two](source-title-2.md)",
		"wiki/source-title.md":   "---\nsource_url: " + urls[0] + "\n---\n# Title\nFirst",
		"wiki/source-title-2.md": "---\nsource_url: " + urls[1] + "\n---\n# Title\nSecond",
		"wiki/log.md":            "# Log\nAdded two sources",
	}
	publish := func(j fixtureJob) {
		t.Helper()

		ma.SetOutput(j.Run.Ref.ID, outputArchive(t, pages))
		ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
		if j = reconcileManaged(t, f, j); j.Status != "completed" {
			t.Fatal(j)
		}
	}
	publish(startManaged(t, f, token, p.ID, false))
	putText(urls[0], "# Title\nFirst\nUpdated from same URL")
	j := startManaged(t, f, token, p.ID, false)
	b := ma.Input(j.Run.Ref.InputFileID)
	input := readInput(t, b)
	for name, body := range pages {
		if input[name] != body {
			t.Fatalf("previous page changed: %s", name)
		}
	}
	pages["wiki/source-title.md"] += "\nUpdated from same URL"
	pages["wiki/log.md"] = "# Log\nUpdated one source"
	publish(j)
	if err := f.stateStore.Read(context.Background(), func(st *store.Queries) error {
		r := fixtureRevisionsGet(t, st, p.ID, fixtureProjectsGet(t, st, p.ID).Revision)
		count := 0
		for name, key := range r.Files {
			if !strings.HasPrefix(name, "wiki/source-") {
				continue
			}
			count++
			body, err := f.s.Blobs.Get(t.Context(), key)
			if err != nil || string(body) != pages[name] {
				t.Fatalf("published source %s: %q, %v", name, body, err)
			}
		}
		if count != 2 || len(fixtureInboxList(t, st, p.ID)) != 0 {
			t.Fatal(count, fixtureInboxList(t, st, p.ID))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLocalSourceRoundTrip(t *testing.T) {
	t.Parallel()
	f, token, p, ma := managedFixture(t)
	const stamp = "updated_at: 2026-11-20T15:00:00Z\n"
	const key = "kioku:local/5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	endpoint := "/api/projects/" + p.ID + "/put"
	content := func(u, body string) string {
		return "---\n" + stamp + "title: 1on1\nsource_url: " + u + "\n---\n" + body
	}
	put := func(body string) string {
		t.Helper()
		var result model.PutResponse
		if c := f.request("POST", endpoint, token, model.PutRequest{File: &model.FileInput{Name: "a.md", Content: body}}, &result); c != 202 {
			t.Fatal(c)
		}
		return result.SourceURL
	}
	for _, tc := range []struct{ body, errorPart string }{
		{content("kioku:local/"+strings.Repeat("0", 64), "hello\n"), "local keys are only for remove"},
		{"---\n" + stamp + "---\nhello\n", "source_url must be a nonempty string"},
		{content("kioku:local", " \n"), "source body after frontmatter is empty"},
		{content("kioku:local/abcd", "hello\n"), "must be HTTPS"},
	} {
		var result struct{ Error string }
		if c := f.request("POST", endpoint, token, model.PutRequest{File: &model.FileInput{Name: "a.md", Content: tc.body}}, &result); c != 400 || !strings.Contains(result.Error, tc.errorPart) {
			t.Fatal(c, result)
		}
	}
	if got := put(content("kioku:local", "hello\n")); got != key {
		t.Fatal(got)
	}
	item := waitingInboxItem(t, f, p.ID, key)
	if body, err := f.s.Blobs.Get(t.Context(), fixtureInboxKey(item)); err != nil || string(body) != content(key, "hello\n") {
		t.Fatalf("HTTP stored body: %q, %v", body, err)
	}
	access := connectorAccessToken(t, f, token, p.ID)
	var mcp model.PutResponse
	args := map[string]any{"project": p.Namespace, "name": "other.md", "content": strings.Replace(content("kioku:local", "hello\n"), stamp, "updated_at: [untrusted, time]\n", 1)}
	before := time.Now()
	if msg := connectorTool(t, f, access, "source_put", args, &mcp); msg != "" || mcp.SourceURL != key {
		t.Fatal(msg, mcp)
	}
	item = waitingInboxItem(t, f, p.ID, key)
	body, err := f.s.Blobs.Get(t.Context(), fixtureInboxKey(item))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := source.Frontmatter(string(body))
	if err != nil {
		t.Fatal(err)
	}
	var frontmatter struct {
		UpdatedAt string `yaml:"updated_at"`
	}
	if err := meta.Decode(&frontmatter); err != nil {
		t.Fatal(err)
	}
	received, err := time.Parse(time.RFC3339Nano, frontmatter.UpdatedAt)
	if err != nil || received.Before(before) || received.After(time.Now()) || !strings.HasSuffix(string(body), "---\nhello\n") {
		t.Fatalf("MCP stored body: %q, %v", body, err)
	}

	const https = "https://example.com/old"
	if got := put(content(https, "Old\n")); got != "" {
		t.Fatal(got)
	}
	pages := map[string]string{"wiki/index.md": "# Wiki", "wiki/log.md": "# Log", "wiki/source-old.md": content(https, "Old\n"), "wiki/source-1on1.md": content(key, "hello\n")}
	publish := func() {
		t.Helper()
		j := startManaged(t, f, token, p.ID, false)
		ma.SetOutput(j.Run.Ref.ID, outputArchive(t, pages))
		ma.SetStatus(j.Run.Ref.ID, "idle", "end_turn", 1)
		if j = reconcileManaged(t, f, j); j.Status != "completed" {
			t.Fatal(j)
		}
	}
	publish()
	if got := put(content("kioku:local", "hello\n")); got != key {
		t.Fatal(got)
	}
	second := put(content("kioku:local", "world\n"))
	pages["wiki/source-1on1-2.md"] = content(second, "world\n")
	publish()

	var result map[string]any
	if c := f.request("POST", endpoint, token, model.PutRequest{Remove: true, SourceURL: key}, &result); c != 202 || len(result) != 0 {
		t.Fatal(c, result)
	}
	delete(pages, "wiki/source-1on1.md")
	publish()
}
