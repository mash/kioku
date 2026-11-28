package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/server"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/internal/wiki"
	"github.com/mash/kioku/prompts"
)

// TestLocalSourceV10Integration uses the real queue, guard, CLI, SQLite inbox
// and publication code. Connector replies and the runner's writing are fixtures.
func TestLocalSourceV10Integration(t *testing.T) {
	home := sessionTestEnv(t)
	t.Setenv(captureModeEnv, "")
	project := t.TempDir()
	db, err := store.OpenDatabase(filepath.Join(t.TempDir(), "catalog.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := prompts.Load(prompts.Variables{ClientVersion: server.MinimumClientVersion, MaxOutputBytes: 128 << 20}, server.MCPToolNames())
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(server.Config{Prompts: rendered, GenerateDebounce: time.Hour}, db, local.Store{Dir: t.TempDir()}, key)
	fixture := &localSourceWikiFixture{}
	s.SetRunner(fixture)
	httpServer := httptest.NewUnstartedServer(s.Handler())
	s.Config.BaseURL = "http://" + httpServer.Listener.Addr().String()
	httpServer.Start()
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	done := make(chan error, 1)
	go func() { done <- s.RunWorker(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	token := auth.Token()
	if err := db.Write(ctx, func(q *store.Queries) error {
		if err := q.PutUser(store.UserRow{ID: "v10-user", Email: "v10@example.com", Data: store.UserData{Name: "Fixture"}}); err != nil {
			return err
		}
		if err := q.PutIdentity("v10-user", "test", "v10"); err != nil {
			return err
		}
		raw, err := json.Marshal(store.IdentitySessionData{})
		if err != nil {
			return err
		}
		return q.PutSession(store.SessionRow{ID: auth.Hash(token), Kind: "user", Status: "active", UserID: "v10-user", Expires: time.Now().Add(time.Hour).UnixMilli(), Data: raw})
	}); err != nil {
		t.Fatal(err)
	}
	a := &App{
		Dir: project, AuthDir: filepath.Join(home, ".config/kioku/auth"),
		Config: Config{Server: httpServer.URL, Namespace: "v10/docs", Token: token},
		Client: httpServer.Client(), ctx: ctx, Out: io.Discard, Err: io.Discard,
	}
	if err := a.saveCredentials(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		actual := append([]string{"-C", project, "--server", httpServer.URL}, args...)
		if err := Run(ctx, actual, &out, &stderr); err != nil {
			t.Fatalf("%v: %v, %s %s", args, err, out.String(), stderr.String())
		}
		return out.String()
	}
	run("org", "create", "v10")
	run("-n", "v10/docs", "project", "create")
	if err := a.selectCredential(false); err != nil {
		t.Fatal(err)
	}
	fakeCaptureClaude(t, "backgrounded · 1a2b3c4d\n", 0)

	const web = "https://example.com/original"
	const artifact = "https://claude.ai/artifact/fixture"
	const notion = "https://app.notion.com/p/fixture"
	const linear = "https://linear.app/team/issue/FIX-4"
	const artifactName = "wiki/source-artifact.md"
	const notionTool = "mcp__claude_ai_Notion__notion-fetch"
	const linearTool = "mcp__linear__get_issue"
	const metricTool = "mcp__datadog__get_metric"
	type item struct {
		name, input, response, url, updated, text string
	}
	artifactItem := item{"Artifact", `{"file_path":"fixture.html"}`,
		`"Published «fixture» at https://claude.ai/artifact/fixture (Version 1, version id 1790000000-aaaa)"`,
		artifact, "", "First artifact"}
	webItem := item{"WebFetch", `{"url":"https://example.com/original"}`, "", web, "2026-11-28T08:00:00Z", "Web fixture"}
	document := item{notionTool, `{"id":"fixture"}`, "", notion, "2026-11-28T08:01:00Z", "Notion fixture"}
	artifactReadTime := func(reply string) string {
		t.Helper()
		match := regexp.MustCompile(`from version ([0-9]{10})-[0-9a-f]+\.`).FindStringSubmatch(reply)
		if len(match) != 2 {
			t.Fatal("Artifact read fixture version id format changed", reply)
		}
		seconds, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
	}
	artifactItem.updated = artifactReadTime(`File saved: "/fixture/read.html" (5 bytes, "text/html", sha256 fixture) — "index.html" from version 1790000000-aaaa.`)
	issue := item{linearTool, `{"id":"FIX-4"}`, "", linear, "2026-11-28T08:02:00Z", "Linear fixture"}
	metric := item{metricTool, `{"id":42}`, "", "", "", ""}
	post := func(it item) string {
		t.Helper()
		ev := hookToolEvent{Event: "PostToolUse", SessionID: "session-1", Cwd: project, Name: it.name, Input: json.RawMessage(it.input)}
		if it.response != "" {
			ev.Response = json.RawMessage(it.response)
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		hookTool(ctx, bytes.NewReader(payload))
		src := toolHookSource(ctx, ev)
		if src == "" {
			t.Fatal("no source from fixture", it.name)
		}
		return src
	}
	readQueueBytes := func() []byte {
		t.Helper()
		b, err := os.ReadFile(a.queuePath())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	wSrc, aSrc, nSrc, iSrc, mSrc := post(webItem), post(artifactItem), post(document), post(issue), post(metric)
	before := readQueueBytes()
	post(artifactItem)
	post(webItem)
	if !bytes.Equal(before, readQueueBytes()) {
		t.Fatal("duplicate registration changed queued rows")
	}

	start := func() (string, string, []queueRow) {
		t.Helper()
		run("capture", "run")
		lock, err := readCaptureLock(a.captureLockPath())
		if err != nil || lock == nil || lock.BgID != "1a2b3c4d" {
			t.Fatal(lock, err)
		}
		dir, rows, err := captureSnapshot(lock.Token, a.captureTmp(lock.Token))
		if err != nil {
			t.Fatal(err)
		}
		return lock.Token, dir, rows
	}
	putArgs := func(token, dir, src string, it item) []string {
		t.Helper()
		path := filepath.Join(dir, "source.md")
		content := fmt.Sprintf("---\nsource_url: %s\nupdated_at: %s\n---\n%s\n", it.url, it.updated, it.text)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return []string{"capture", "put", "--capture-token", token, "--source", src, path}
	}
	result := func(token, src, status, reason string) {
		t.Helper()
		args := []string{"capture", "result", "--capture-token", token, "--result", status}
		if reason != "" {
			args = append(args, "--reason", reason)
		}
		run(append(args, src)...)
	}
	cToken, dir, rows := start()
	if len(rows) != 5 {
		t.Fatal(rows)
	}
	for _, tc := range []struct {
		src string
		it  item
	}{{wSrc, webItem}, {aSrc, artifactItem}, {nSrc, document}} {
		toolName, input := tc.it.name, tc.it.input
		if toolName == "Artifact" {
			input = fmt.Sprintf(`{"action":"read","url":%q,"path":%q}`, artifact, filepath.Join(dir, "artifact.html"))
		}
		if toolName != "WebFetch" && !allowCaptureTool(hookToolEvent{Name: toolName, Input: json.RawMessage(input)}, dir, rows) {
			t.Fatal("guard blocked fixture read", toolName)
		}
		if out := run(putArgs(cToken, dir, tc.src, tc.it)...); out != "accepted "+tc.it.url+"\n" {
			t.Fatal(out)
		}
		result(cToken, tc.src, "accepted", "")
	}
	if out := run("delete", iSrc); out != "removed "+iSrc+"\n" {
		t.Fatal(out)
	}
	if out := run("delete", iSrc); out != "" {
		t.Fatal("reported removal of an already absent tool source", out)
	}
	post(issue)
	oldArgs := putArgs(cToken, dir, iSrc, issue)
	var out bytes.Buffer
	if err := Run(ctx, append([]string{"-C", project, "--server", httpServer.URL}, oldArgs...), &out, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "再登録") || out.Len() != 0 {
		t.Fatal(out.String(), err)
	}
	result(cToken, iSrc, "skipped", "deleted fixture")
	result(cToken, mSrc, "skipped", "numbers only")
	if status := run("status"); !strings.Contains(status, `"capture": null`) || !strings.Contains(status, `"inbox_count": 3`) {
		t.Fatal(status)
	}
	log, err := os.ReadFile(filepath.Join(a.metadataDir(), "capture.log"))
	if err != nil || !strings.Contains(string(log), "accepted "+notion) || !strings.Contains(string(log), "numbers only") {
		t.Fatal(string(log), err)
	}
	publish := func() {
		t.Helper()
		// The production reconcile cadence is intentionally slower. Drive the
		// same reconciliation entry point on a short fixture clock.
		var job model.Job
		if err := json.Unmarshal([]byte(run("generate")), &job); err != nil || job.ID == "" {
			t.Fatal(job, err)
		}
		waitCtx, stop := context.WithCancel(ctx)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-waitCtx.Done():
					return
				case <-ticker.C:
					for _, id := range fixture.startedJobs() {
						s.ReconcileJob(waitCtx, id)
					}
				}
			}
		}()
		err := a.wait(job)
		stop()
		<-finished
		if err != nil {
			t.Fatal(err)
		}
	}
	publish()
	checkPage := func(path, fragment string) {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(project, path))
		if err != nil || !strings.Contains(string(body), fragment) {
			t.Fatal(path, string(body), err)
		}
	}
	checkPage(artifactName, "First artifact")
	checkPage("wiki/source-notion.md", notion)
	checkPage("wiki/source-web.md", web)

	artifactItem.input = `{"action":"publish","file_path":"fixture.html","url":"https://claude.ai/artifact/fixture"}`
	artifactItem.response = `"Published «fixture» at https://claude.ai/artifact/fixture (Version 2, version id 1790000001-bbbb)"`
	artifactItem.text = "New artifact content"
	artifactItem.updated = artifactReadTime(`File saved: "/fixture/read.html" (19 bytes, "text/html", sha256 fixture) — "index.html" from version 1790000001-bbbb.`)
	post(artifactItem)
	cToken, dir, rows = start()
	if len(rows) != 2 {
		t.Fatal(rows)
	}
	for _, tc := range []struct {
		src string
		it  item
	}{{aSrc, artifactItem}, {iSrc, issue}} {
		run(putArgs(cToken, dir, tc.src, tc.it)...)
		result(cToken, tc.src, "accepted", "")
	}
	publish()
	checkPage(artifactName, "New artifact content")
	checkPage(artifactName, artifactItem.updated)
	checkPage("wiki/source-linear.md", linear)
	if out := run("delete", notion); out != "removed "+notion+"\n" {
		t.Fatal(out)
	}
	publish()
	if _, err := os.Stat(filepath.Join(project, "wiki/source-notion.md")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	checkPage(artifactName, "New artifact content")
}

type localSourceWikiFixture struct {
	mu      sync.Mutex
	outputs map[string][]byte
}

func (f *localSourceWikiFixture) startedJobs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	jobs := sortedKeys(f.outputs)
	for i := range jobs {
		jobs[i] = strings.TrimPrefix(jobs[i], "test-")
	}
	return jobs
}

func (*localSourceWikiFixture) FilePaths() (string, string)                   { return "input.tar.gz", "output.tar.gz" }
func (*localSourceWikiFixture) Stop(context.Context, runner.Attempt) error    { return nil }
func (*localSourceWikiFixture) Cleanup(context.Context, runner.Attempt) error { return nil }
func (*localSourceWikiFixture) Inspect(_ context.Context, a runner.Attempt) (runner.Status, error) {
	return runner.Status{State: runner.StateStopped, Run: a.Run}, nil
}
func (f *localSourceWikiFixture) CollectOutput(_ context.Context, a runner.Attempt, _ runner.Status, _ int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.outputs[a.Run.ID]
	if !ok {
		return nil, errors.New("missing generated fixture")
	}
	return data, nil
}

func (f *localSourceWikiFixture) Start(_ context.Context, spec runner.RunSpec) (runner.RunRef, error) {
	files, err := readV10FixtureArchive(spec.InputArchive)
	if err != nil {
		return runner.RunRef{}, err
	}
	pages := map[string]string{"wiki/index.md": "# Fixture", "wiki/log.md": "# Test generation"}
	names := map[string]string{
		"https://example.com/original":        "wiki/source-web.md",
		"https://claude.ai/artifact/fixture":  "wiki/source-artifact.md",
		"https://app.notion.com/p/fixture":    "wiki/source-notion.md",
		"https://linear.app/team/issue/FIX-4": "wiki/source-linear.md",
	}
	for name, content := range files {
		if strings.HasPrefix(name, "wiki/source-") {
			pages[name] = content
		}
	}
	for name, content := range files {
		if !strings.HasPrefix(name, "inbox/") {
			continue
		}
		url, err := wiki.SourceURL(content)
		target := names[url]
		if err != nil || target == "" {
			return runner.RunRef{}, fmt.Errorf("fixture has unexpected source %q: %v", url, err)
		}
		header, body, ok := strings.Cut(strings.TrimPrefix(content, "---\n"), "\n---\n")
		if !ok {
			return runner.RunRef{}, errors.New("fixture missing outer frontmatter")
		}
		if strings.Contains(header, "\nremove: true") {
			delete(pages, target)
		} else {
			pages[target] = body
		}
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, name := range sortedKeys(pages) {
		body := pages[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}); err != nil {
			return runner.RunRef{}, err
		}
		if _, err := io.WriteString(tw, body); err != nil {
			return runner.RunRef{}, err
		}
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		return runner.RunRef{}, err
	}
	ref := runner.RunRef{UsesProviderFiles: true, ID: "test-" + spec.Key.JobID, InputFileID: "fixture", UploadStarted: true}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outputs == nil {
		f.outputs = map[string][]byte{}
	}
	f.outputs[ref.ID] = b.Bytes()
	return ref, nil
}

func readV10FixtureArchive(data []byte) (map[string]string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[h.Name] = string(body)
	}
}
