package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/mash/kioku/internal/source"
)

// captureToolCall is the JSON payload of a tool: source. The order of these
// fields is also the canonical order used on disk.
type captureToolCall struct {
	Input json.RawMessage `json:"input"`
	Tool  string          `json:"tool"`
}

const toolSourcePrefix = "tool:"

func captureToolSource(name string, input json.RawMessage) (string, error) {
	if !isMCPReadTool(name) {
		return "", errors.New("not a capture read tool")
	}
	data, err := canonicalToolInput(input)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(captureToolCall{Input: data, Tool: name})
	if err != nil {
		return "", err
	}
	return toolSourcePrefix + string(out), nil
}

func canonicalToolInput(input json.RawMessage) (json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(input))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("expected one tool input JSON value")
	}
	return json.Marshal(v)
}

// parseCaptureSource returns a nil call for a valid HTTPS identity.
func parseCaptureSource(raw string) (*captureToolCall, error) {
	payload, tool := strings.CutPrefix(raw, toolSourcePrefix)
	if !tool {
		return nil, source.ValidateHTTPS(raw)
	}
	var call captureToolCall
	if err := json.Unmarshal([]byte(payload), &call); err != nil {
		return nil, err
	}
	canonical, err := captureToolSource(call.Tool, call.Input)
	if err != nil {
		return nil, err
	}
	if canonical != raw {
		return nil, errors.New("tool source must be in canonical form")
	}
	return &call, nil
}

func mcpToolParts(name string) (server, tool string) {
	tail, ok := strings.CutPrefix(name, "mcp__")
	at := strings.LastIndex(tail, "__")
	if !ok || at <= 0 || at+2 == len(tail) || strings.ContainsAny(tail, " \t\r\n") {
		return "", ""
	}
	return tail[:at], tail[at+2:]
}

func sameMCPServer(a, b string) bool {
	left, _ := mcpToolParts(a)
	right, _ := mcpToolParts(b)
	return left != "" && left == right
}

var camelToolBoundary = regexp.MustCompile(`([\p{Ll}\p{N}])(\p{Lu})`)
var acronymToolBoundary = regexp.MustCompile(`(\p{Lu})(\p{Lu}\p{Ll})`)

func isMCPReadTool(name string) bool {
	server, tool := mcpToolParts(name)
	if server == "" || strings.HasPrefix(name, "mcp__plugin_kioku_kioku__") {
		return false
	}
	tool = acronymToolBoundary.ReplaceAllString(tool, "${1}_${2}")
	tool = camelToolBoundary.ReplaceAllString(tool, "${1}_${2}")
	read := false
	for _, word := range strings.FieldsFunc(strings.ToLower(tool), func(r rune) bool { return r == '-' || r == '_' }) {
		switch word {
		case "create", "update", "delete", "remove", "send", "post", "write", "put", "add", "set", "edit", "move", "trash", "share", "archive", "publish", "upload", "search", "list":
			return false
		case "get", "fetch", "read", "view", "download":
			read = true
		}
	}
	return read
}
