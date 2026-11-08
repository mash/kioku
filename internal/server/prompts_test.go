package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/agentgen"
	"github.com/mash/kioku/prompts"
)

func defaultPrompts(t *testing.T) *prompts.Rendered {
	t.Helper()
	r, err := prompts.Load("", prompts.Variables{ClientVersion: MinimumClientVersion, MaxOutputBytes: 128 << 20}, MCPToolNames())
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func loadPrompts(t *testing.T, vars prompts.Variables, files map[string]string) (*prompts.Rendered, error) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return prompts.Load(dir, vars, MCPToolNames())
}
func overridePrompts(t *testing.T, f *fixture, system, user, mcp string) {
	t.Helper()
	files := map[string]string{}
	for name, body := range map[string]string{prompts.SystemFile: system, prompts.UserFile: user, prompts.MCPFile: mcp} {
		if body != "" {
			files[name] = body
		}
	}
	rendered, err := loadPrompts(t, prompts.Variables{BaseURL: f.s.Config.BaseURL, ClientVersion: MinimumClientVersion, MaxOutputBytes: f.s.Config.MaxWikiBytes * 2}, files)
	if err != nil {
		t.Fatal(err)
	}
	f.s.Config.Prompts = rendered
	f.s.mcpTools = mcpTools(rendered)
	f.setRunners()
}
func TestMCPPromptOverridesOnWire(t *testing.T) {
	f := setupWithWorker(t, false)
	human := f.login("owner@example.com")
	p := securityProject(t, f, human)
	token := connectorAccessToken(t, f, human, p.ID)
	var before, after struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(connectorRPC(t, f, token, "tools/list", nil), &before); err != nil {
		t.Fatal(err)
	}
	var init map[string]any
	if err := json.Unmarshal(connectorRPC(t, f, token, "initialize", nil), &init); err != nil {
		t.Fatal(err)
	}
	if _, ok := init["instructions"]; ok {
		t.Fatal("default instructions changed")
	}
	overridePrompts(t, f, "", "", "instructions: 'Read our {{.BaseURL}} guidance'\ntools:\n  wiki_read:\n    description: Custom wiki reader\n    properties:\n      path:\n        description: Custom path\n")
	if err := json.Unmarshal(connectorRPC(t, f, token, "initialize", nil), &init); err != nil {
		t.Fatal(err)
	}
	if init["instructions"] != "Read our "+f.s.Config.BaseURL+" guidance" {
		t.Fatal(init)
	}
	if err := json.Unmarshal(connectorRPC(t, f, token, "tools/list", nil), &after); err != nil {
		t.Fatal(err)
	}
	tool := after.Tools[2]
	if tool["description"] != "Custom wiki reader" {
		t.Fatal(tool)
	}
	path := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)["path"].(map[string]any)
	if path["description"] != "Custom path" {
		t.Fatal(path)
	}
	tool["description"] = before.Tools[2]["description"]
	delete(path, "description")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("MCP schema, order, or other descriptions changed")
	}
}
func TestUserOverrideKeepsJOBMetadata(t *testing.T) {
	f, token, p, ma := managedFixture(t)
	overridePrompts(t, f, "", "Read {{.InputURL}} and write {{.OutputURL}} for {{.JobID}} / {{.Namespace}}", "")
	j := startManaged(t, f, token, p.ID, true)
	base := f.s.Config.BaseURL + "/api/projects/" + p.Namespace + "/jobs/" + j.ID + "/agent/"
	found := false
	for _, req := range ma.Requests() {
		if req.Path == "/v1/sessions" && req.Method == "POST" {
			found = true
			text := req.Body["initial_events"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
			if text != "Read "+base+"input and write "+base+"output for "+j.ID+" / "+p.Namespace {
				t.Fatal(text)
			}
		}
	}
	if !found {
		t.Fatal("session not created")
	}
	data, err := f.s.Blobs.Get(t.Context(), agentJobKey(j, "input.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	files := readInput(t, data)
	want := agentgen.JobMetadata(agentgen.InputSpec{JobID: j.ID, Namespace: p.Namespace, InputURL: base + "input", OutputURL: base + "output"})
	if files["JOB.md"] != want || strings.Contains(files["JOB.md"], "Read ") {
		t.Fatal(files["JOB.md"])
	}
}

func TestMCPUnknownNamesStopLoading(t *testing.T) {
	for _, body := range []string{
		"tools: {private-secret: {description: hi}}",
		"tools: {private-secret: []}",
		"tools: {wiki_read: {properties: {private-secret: {description: hi}}}}",
		"tools: {wiki_read: {properties: {private-secret: []}}}",
	} {
		t.Run(body, func(t *testing.T) {
			_, err := loadPrompts(t, prompts.Variables{}, map[string]string{prompts.MCPFile: body})
			if err == nil || err.Error() != "prompts: mcp-server.yaml: unknown key (line 1)" {
				t.Fatal(err)
			}
		})
	}
}
