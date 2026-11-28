package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mash/kioku/internal/source"
)

const (
	captureModeEnv           = "KIOKU_CAPTURE"
	captureDirEnv            = "KIOKU_CAPTURE_DIR"
	captureCommandEnv        = "KIOKU_CAPTURE_COMMAND"
	captureProjectCommandEnv = "KIOKU_CAPTURE_PROJECT_COMMAND"
)

type hookToolEvent struct {
	Event     string          `json:"hook_event_name"`
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	Name      string          `json:"tool_name"`
	Input     json.RawMessage `json:"tool_input"`
	Response  json.RawMessage `json:"-"`
}

const maxHookToolInputBytes = 8 << 20
const captureGuardMatcher = "^(Artifact|mcp__.*|Bash|Read|Write)$"

func decodeHookEvent(input io.Reader, event any) error {
	d := json.NewDecoder(input)
	if err := d.Decode(event); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one hook event")
	}
	return nil
}

func (e *hookToolEvent) UnmarshalJSON(data []byte) error {
	type fields hookToolEvent
	if err := json.Unmarshal(data, (*fields)(e)); err != nil {
		return err
	}
	if len(e.Input) > maxHookToolInputBytes {
		return errors.New("tool_input exceeds 8 MiB")
	}
	if e.Event == "PostToolUse" && e.Name == "Artifact" {
		var arg artifactInput
		if json.Unmarshal(e.Input, &arg) == nil && arg.isPublish() {
			return json.Unmarshal(data, &struct {
				Response *json.RawMessage `json:"tool_response"`
			}{Response: &e.Response})
		}
	}
	return nil
}

func hookToolGuard(input io.Reader, output io.Writer) error {
	if os.Getenv(captureModeEnv) != "1" {
		return nil
	}
	var event hookToolEvent
	dir := os.Getenv(captureDirEnv)
	if decodeHookEvent(input, &event) == nil && event.Event == "PreToolUse" {
		resolved, rows, err := captureSnapshot(filepath.Base(dir), dir)
		if err == nil && allowCaptureTool(event, resolved, rows) {
			return nil
		}
	}
	return json.NewEncoder(output).Encode(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "Tool is outside the Kioku capture scope",
	}})
}

type artifactInput struct {
	Action   string `json:"action"`
	URL      string `json:"url"`
	OutDir   string `json:"out_dir"`
	FilePath string `json:"file_path"`
}

// The native tool also uses implicit publish with a file_path.
func (a artifactInput) isPublish() bool {
	return a.Action == "publish" || a.Action == "" && a.FilePath != ""
}

func isArtifactURL(raw string) bool {
	if source.ValidateHTTPS(raw) != nil {
		return false
	}
	u, _ := url.Parse(raw)
	return u.Host == "claude.ai" && (strings.HasPrefix(u.Path, "/artifact/") || strings.HasPrefix(u.Path, "/code/artifact/") || strings.HasPrefix(u.Path, "/public/artifacts/"))
}

func allowCaptureTool(in hookToolEvent, dir string, rows []queueRow) bool {
	switch in.Name {
	case "Artifact":
		var arg artifactInput
		return json.Unmarshal(in.Input, &arg) == nil && arg.Action == "read" &&
			arg.FilePath == "" && isArtifactURL(arg.URL) && snapshotRow(rows, arg.URL) >= 0 &&
			captureArtifactOutDir(dir, arg.OutDir)
	case "Read", "Write":
		var arg struct {
			Path string `json:"file_path"`
		}
		return json.Unmarshal(in.Input, &arg) == nil && captureToolPath(dir, arg.Path, in.Name == "Write")
	case "Bash":
		var arg struct {
			Command string `json:"command"`
		}
		return json.Unmarshal(in.Input, &arg) == nil && captureBashAllowed(arg.Command, dir, rows)
	default:
		if !isMCPReadTool(in.Name) || len(in.Input) == 0 || !utf8.Valid(in.Input) {
			return false
		}
		// The same-server read rule also covers an exact replay.
		for _, row := range rows {
			if sameMCPServer(captureSourceTool(row.Source), in.Name) {
				return true
			}
		}
		return false
	}
}

func captureArtifactOutDir(dir, out string) bool {
	if out == "" || out == dir {
		return true
	}
	if !captureToolPath(dir, out, true) {
		return false
	}
	info, err := os.Lstat(out)
	return os.IsNotExist(err) || err == nil && info.IsDir()
}

func captureToolPath(dir, path string, writing bool) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return false
	}
	if writing && (strings.EqualFold(rel, captureSnapshotFile) || strings.EqualFold(rel, captureInstructionsFile)) {
		return false
	}
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		if os.IsNotExist(err) {
			return writing
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !(info.IsDir() || info.Mode().IsRegular()) {
			return false
		}
	}
	return true
}

func captureBashAllowed(command, dir string, rows []queueRow) bool {
	args, ok := literalShellWords(command)
	if !ok {
		return false
	}
	base, ok := literalShellWords(os.Getenv(captureCommandEnv))
	if !ok || !filepath.IsAbs(base[0]) {
		return false
	}
	token := filepath.Base(dir)
	if after := trimCommandPrefix(args, base, "capture", "fetch"); len(after) == 7 {
		return after[1] == "--capture-token" && after[2] == token &&
			after[3] == "--dir" && after[4] == dir && after[5] == "--out" &&
			captureNeedsFetch(after[0]) &&
			snapshotRow(rows, after[0]) >= 0 && captureToolPath(dir, after[6], true)
	}
	project, ok := literalShellWords(os.Getenv(captureProjectCommandEnv))
	if !ok || !slices.Equal(project[:min(len(project), len(base))], base) {
		return false
	}
	if after := trimCommandPrefix(args, project, "capture", "put", "--capture-token", token, "--source"); len(after) == 2 {
		return snapshotRow(rows, after[0]) >= 0 && captureToolPath(dir, after[1], false)
	}
	if after := trimCommandPrefix(args, project, "capture", "result", "--capture-token", token, "--result"); len(after) == 2 || len(after) == 4 {
		result := after[0]
		src := after[len(after)-1]
		if snapshotRow(rows, src) < 0 {
			return false
		}
		if len(after) == 2 {
			return result == "accepted"
		}
		return after[1] == "--reason" && strings.TrimSpace(after[2]) != "" &&
			(result == "unsaved" || result == "skipped")
	}
	return false
}

func trimCommandPrefix(args, prefix []string, rest ...string) []string {
	n, m := len(prefix), len(rest)
	if len(args) < n+m || !slices.Equal(args[:n], prefix) || !slices.Equal(args[n:n+m], rest) {
		return nil
	}
	return args[n+m:]
}

// literalShellWords accepts one simple shell command with literal arguments.
// It never evaluates shell expansions or interprets operators.
func literalShellWords(line string) ([]string, bool) {
	if strings.ContainsAny(line, "\x00\r\n") {
		return nil, false
	}
	var args []string
	var b strings.Builder
	var quote byte
	active := false
	flush := func() {
		if active {
			args = append(args, b.String())
			b.Reset()
			active = false
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if quote == '"' && strings.ContainsRune("$`\\!", rune(c)) {
				return nil, false
			} else {
				b.WriteByte(c)
			}
			continue
		}
		switch c {
		case ' ', '\t':
			flush()
		case '\'', '"':
			active, quote = true, c
		case '\\':
			if i+1 >= len(line) {
				return nil, false
			}
			i++
			active = true
			b.WriteByte(line[i])
		default:
			if strings.ContainsRune("$`;&|<>()*?[]{}!#~", rune(c)) {
				return nil, false
			}
			active = true
			b.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, false
	}
	flush()
	return args, len(args) > 0 && args[0] != ""
}
