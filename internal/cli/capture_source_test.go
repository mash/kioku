package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCaptureSourceHTTPS(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		allowed bool
	}{
		{"https://example.com/a?private=true#anchor", true},
		{"https://127.0.0.1:8443/share", true},
		{"https://private.local/", true},
		{"https://", false}, {"http://example.com", false},
		{"https://user@example.com/a", false},
		{"https://example.com/a\tb", false}, {"https://example.com/a\nb", false},
		{"https://example.com/" + strings.Repeat("a", 4096), false},
	} {
		call, err := parseCaptureSource(tc.raw)
		if (err == nil) != tc.allowed || call != nil {
			t.Errorf("%q: %v, %v", tc.raw, call, err)
		}
	}
}

func TestCaptureSourceCanonicalInput(t *testing.T) {
	const name = "mcp__claude_ai_Notion__notion-fetch"
	const expected = `tool:{"input":{"a":[{"k":2,"z":1},9007199254740993],"b":"hello\tworld"},"tool":"mcp__claude_ai_Notion__notion-fetch"}`
	for _, raw := range []string{
		`{ "b":"hello\tworld", "a":[ {"z":1,"k":2},9007199254740993 ] }`,
		`{"a":[{"k":2,"z":1},9007199254740993],"b":"hello\tworld"}`,
	} {
		got, err := captureToolSource(name, json.RawMessage(raw))
		if err != nil || got != expected {
			t.Fatal(got, err)
		}
		call, err := parseCaptureSource(got)
		if err != nil || call.Tool != name {
			t.Fatal(call, err)
		}
	}
	for _, raw := range []string{expected + " ", strings.Replace(expected, `"a":`, `"a" :`, 1), `tool:{"tool":"mcp__x__update_record","input":{}}`} {
		if _, err := parseCaptureSource(raw); err == nil {
			t.Fatalf("accepted noncanonical or unsafe source %q", raw)
		}
	}
}

func TestMCPReadTool(t *testing.T) {
	for name, want := range map[string]bool{
		"mcp__s__getJiraIssue": true, "mcp__s__get_design_context": true,
		"mcp__s__READHTTPResponse": false, "mcp__s__readHTTPResponse": true,
		"mcp__s__notion-fetch": true, "mcp__s__slack_read_thread": true,
		"mcp__s__download_file_content": true, "mcp__s__target_issue": false,
		"mcp__s__getIssueList": false, "mcp__s__get_update": false,
		"mcp__s__read_search_results": false, "mcp__s__slack_send_message": false,
		"mcp__plugin_kioku_kioku__wiki_read": false,
		"mcp__get-server__issue":             false, "mcp__nested__server__view_file": true,
		"mcp__get": false, "get_issue": false,
	} {
		if got := isMCPReadTool(name); got != want {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	if !sameMCPServer("mcp__nested__s__get_issue", "mcp__nested__s__read_issue") ||
		sameMCPServer("mcp__nested__s__get_issue", "mcp__s__get_issue") ||
		sameMCPServer("get_issue", "get_issue") {
		t.Fatal("MCP server identity did not use the last separator")
	}
}
