package prompts

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPromptFile = "wiki-agent.default-prompt.md"
	SystemFile        = "wiki-agent.system.md"
	UserFile          = "wiki-agent.user.md"
	MCPFile           = "mcp-server.yaml"
)

// WikiInstructionsParagraph applies owner instructions within the system rules.
const WikiInstructionsParagraph = "WIKI.md in the input holds the project owner's instructions for organizing this wiki. Follow them when you choose, split and structure pages and wiki/index.md. When they conflict with the rules above, follow the rules above."

type Variables struct {
	BaseURL, ClientVersion string
	MaxOutputBytes         int64
}
type JobVariables struct{ JobID, Namespace, InputURL, OutputURL string }
type Model struct{ ID, Effort, Speed, InferenceGeo string }
type Source struct{ File, SHA256 string }

// Rendered owns a startup snapshot. No job or caller can mutate its templates.
type Rendered struct {
	defaultPrompt string
	system        string
	fileSystem    string
	model         Model
	user          textTemplate
	vars          map[string]string
	mcp           MCP
	sources       []Source
}

func (r *Rendered) WikiDefaultPrompt() string { return r.defaultPrompt }
func (r *Rendered) System() string            { return r.system }
func (r *Rendered) FileSystem() string        { return r.fileSystem }
func (r *Rendered) Model() Model              { return r.model }
func (r *Rendered) Sources() []Source         { return slices.Clone(r.sources) }
func (r *Rendered) Log() {
	for _, s := range r.sources {
		slog.Info("prompt loaded", "file", s.File, "sha256", s.SHA256)
	}
}

var jobVariableNames = []string{"JobID", "Namespace", "InputURL", "OutputURL"}

func (r *Rendered) User(job JobVariables) string     { return r.renderUser(job, false) }
func (r *Rendered) FileUser(job JobVariables) string { return r.renderUser(job, true) }
func (r *Rendered) renderUser(job JobVariables, files bool) string {
	values := maps.Clone(r.vars)
	if files {
		values["BaseURL"] = ""
	}
	for i, value := range []string{job.JobID, job.Namespace, job.InputURL, job.OutputURL} {
		values[jobVariableNames[i]] = value
	}
	return r.user.render(values)
}
func invalid(file, kind string, line int) error {
	return fmt.Errorf("prompts: %s: %s (line %d)", file, kind, max(1, line))
}

// Load renders the embedded prompt files. Servers pass their tool/property
// names; callers without an MCP server may pass nil.
func Load(vars Variables, knownTools map[string][]string) (*Rendered, error) {
	return LoadWith(nil, vars, knownTools)
}

// LoadWith renders the embedded prompt files with some of them replaced by
// files, keyed by file name. Tests use it to vary the prompts. The text in
// files[MCPFile] replaces only the descriptions that it names.
func LoadWith(replaced map[string]string, vars Variables, knownTools map[string][]string) (*Rendered, error) {
	files := map[string]string{}
	for _, name := range []string{SystemFile, UserFile, MCPFile, DefaultPromptFile} {
		b, err := defaults.ReadFile(name)
		if err != nil {
			panic(err)
		}
		files[name] = strings.TrimPrefix(string(b), "\ufeff")
	}
	defaultMCP := files[MCPFile]
	for name, body := range replaced {
		if _, ok := files[name]; !ok {
			return nil, invalid(name, "unknown file", 1)
		}
		files[name] = strings.TrimPrefix(body, "\ufeff")
	}
	r := &Rendered{vars: map[string]string{"BaseURL": vars.BaseURL, "ClientVersion": vars.ClientVersion, "MaxOutputBytes": strconv.FormatInt(vars.MaxOutputBytes, 10)}}
	body, line, err := r.loadSystem(files[SystemFile])
	if err != nil {
		return nil, err
	}
	systemVars := maps.Clone(r.vars)
	systemVars["ReadJobInput"] = "Download the input with curl using \"Authorization: Bearer $KIOKU_JOB_TOKEN\" and \"Kioku-Client-Version: " + vars.ClientVersion + "\". On 409, 5xx or connection failure wait 10 seconds and retry, up to 30 times. Extract with tar or Python's tarfile."
	systemVars["SubmitJobOutput"] = "Tar and gzip wiki/ only, upload to the output URL with the same two Kioku headers. On 400 read the problems, fix the output and upload again. On 409 or authentication failure stop; the job may have ended."
	system, _, err := parseText(SystemFile, body, line, systemVars)
	if err != nil {
		return nil, err
	}
	r.system = system.render(systemVars)
	if strings.TrimSpace(r.system) == "" {
		return nil, invalid(SystemFile, "empty body", line)
	}
	r.system += "\n\n" + WikiInstructionsParagraph
	systemVars["BaseURL"] = ""
	systemVars["ReadJobInput"] = "Extract the mounted input archive from the local input path."
	systemVars["SubmitJobOutput"] = "Tar and gzip wiki/ only and atomically rename the completed archive to the local output path."
	r.fileSystem = system.render(systemVars) + "\n\n" + WikiInstructionsParagraph + "\n\nThis session uses local files only. The input and output in the user message are local paths. Read the mounted archive and write the completed archive at the output path, creating its parent directory as needed. These instructions supersede any download or upload steps. Do not use the network or credentials. Finish only after writing the archive."
	r.defaultPrompt = files[DefaultPromptFile]
	if isFrontmatterDelimiter(r.defaultPrompt) {
		return nil, invalid(DefaultPromptFile, "syntax", 1)
	}
	if strings.TrimSpace(r.defaultPrompt) == "" {
		return nil, invalid(DefaultPromptFile, "empty body", 1)
	}
	user := files[UserFile]
	if isFrontmatterDelimiter(user) {
		return nil, invalid(UserFile, "syntax", 1)
	}
	var fields map[string]bool
	r.user, fields, err = parseText(UserFile, user, 1, r.vars)
	if err != nil {
		return nil, err
	}
	// Job values arrive at runtime; use non-empty placeholders for validation.
	if strings.TrimSpace(r.User(JobVariables{JobID: "job", Namespace: "project", InputURL: "input", OutputURL: "output"})) == "" {
		return nil, invalid(UserFile, "empty body", 1)
	}
	if !fields["InputURL"] || !fields["OutputURL"] {
		return nil, invalid(UserFile, "missing URL", 1)
	}
	r.mcp.Tools = map[string]MCPTool{}
	if err := r.loadMCP(defaultMCP, knownTools); err != nil {
		return nil, err
	}
	if _, ok := replaced[MCPFile]; ok {
		if err := r.loadMCP(files[MCPFile], knownTools); err != nil {
			return nil, err
		}
	}
	// Hash effective text/configuration, independent of YAML formatting and source.
	r.sources = []Source{
		{SystemFile, hash(struct {
			System string
			Model  Model
		}{r.system, r.model})},
		{UserFile, hash(struct {
			Template  string
			Variables map[string]string
		}{user, r.vars})},
		{MCPFile, hash(r.mcp)},
		{DefaultPromptFile, fmt.Sprintf("%x", sha256.Sum256([]byte(r.defaultPrompt)))},
	}
	return r, nil
}

func hash(value any) string {
	b, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

type textPart struct{ text, name string }
type textTemplate []textPart

func (t textTemplate) render(values map[string]string) string {
	var b strings.Builder
	for _, part := range t {
		b.WriteString(part.text)
		if part.name != "" {
			b.WriteString(values[part.name])
		}
	}
	return b.String()
}

var action = regexp.MustCompile(`^{{(?:\.([A-Za-z_][A-Za-z0-9_]*)|"{{")}}`)

// parseText is the only template validator. It accepts exact field references
// and the literal opening delimiter; replacement values are never parsed again.
func parseText(file, body string, line int, values map[string]string) (textTemplate, map[string]bool, error) {
	var parts textTemplate
	fields := map[string]bool{}
	for {
		start := strings.Index(body, "{{")
		if start < 0 {
			parts = append(parts, textPart{text: body})
			return parts, fields, nil
		}
		parts = append(parts, textPart{text: body[:start]})
		// Decoded YAML newlines do not map to source lines; keep the value's start.
		if file != MCPFile {
			line += strings.Count(body[:start], "\n")
		}
		body = body[start:]
		match := action.FindStringSubmatch(body)
		if match == nil {
			return nil, nil, invalid(file, "forbidden syntax", line)
		}
		name := match[1]
		if name == "" {
			parts = append(parts, textPart{text: "{{"})
		} else {
			if slices.Contains(jobVariableNames, name) {
				if file != UserFile {
					return nil, nil, invalid(file, "per-job variable", line)
				}
			} else if _, ok := values[name]; !ok {
				return nil, nil, invalid(file, "undefined variable", line)
			}
			fields[name] = true
			parts = append(parts, textPart{name: name})
		}
		body = body[len(match[0]):]
	}
}

var errorLine = regexp.MustCompile(`line ([0-9]+)`)

func syntaxError(file string, err error, offset int) error {
	line := 1
	if m := errorLine.FindStringSubmatch(err.Error()); m != nil {
		line, _ = strconv.Atoi(m[1])
	}
	return invalid(file, "syntax", line+offset-1)
}
func readYAML(file, body string, line int) (*yaml.Node, error) {
	d := yaml.NewDecoder(strings.NewReader(body))
	var root yaml.Node
	if err := d.Decode(&root); err != nil {
		if err == io.EOF && file == SystemFile {
			return &yaml.Node{Kind: yaml.MappingNode}, nil
		}
		return nil, syntaxError(file, err, line)
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		return nil, invalid(file, "syntax", line)
	}
	return root.Content[0], nil
}

// mapping rejects duplicate keys, aliases, merges and non-string keys without echoing YAML.
func mapping(file string, n *yaml.Node, offset int) (map[string]*yaml.Node, error) {
	if n.Kind != yaml.MappingNode {
		return nil, invalid(file, "syntax", n.Line+offset)
	}
	out := map[string]*yaml.Node{}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || out[k.Value] != nil {
			return nil, invalid(file, "syntax", k.Line+offset)
		}
		out[k.Value] = v
	}
	return out, nil
}
func scalar(file string, n *yaml.Node, offset int) (string, error) {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return "", invalid(file, "syntax", n.Line+offset)
	}
	return n.Value, nil
}
func (r *Rendered) text(n *yaml.Node) (string, error) {
	value, err := scalar(MCPFile, n, 0)
	if err != nil {
		return "", err
	}
	t, _, err := parseText(MCPFile, value, n.Line, r.vars)
	if err != nil {
		return "", err
	}
	return t.render(r.vars), nil
}
func isFrontmatterDelimiter(text string) bool {
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimRight(line, " \t\r") == "---"
}

func (r *Rendered) loadSystem(text string) (string, int, error) {
	lines := strings.SplitAfter(text, "\n")
	if !isFrontmatterDelimiter(lines[0]) {
		return text, 1, nil
	}
	end := 1
	for ; end < len(lines); end++ {
		if isFrontmatterDelimiter(lines[end]) {
			break
		}
	}
	if end == len(lines) {
		return "", 0, invalid(SystemFile, "syntax", 1)
	}
	front := strings.Join(lines[1:end], "")
	if strings.TrimSpace(front) == "" {
		return strings.Join(lines[end+1:], ""), end + 2, nil
	}
	n, err := readYAML(SystemFile, front, 2)
	if err != nil {
		return "", 0, err
	}
	entries, err := mapping(SystemFile, n, 1)
	if err != nil {
		return "", 0, err
	}
	v := entries["model"]
	delete(entries, "model")
	for _, extra := range entries {
		return "", 0, invalid(SystemFile, "unknown key", extra.Line+1)
	}
	if v != nil {
		if v.Kind == yaml.MappingNode {
			fields, err := mapping(SystemFile, v, 1)
			if err != nil {
				return "", 0, err
			}
			for key, value := range fields {
				var target *string
				switch key {
				case "id":
					target = &r.model.ID
				case "effort":
					target = &r.model.Effort
				case "speed":
					target = &r.model.Speed
				case "inference_geo":
					target = &r.model.InferenceGeo
				default:
					return "", 0, invalid(SystemFile, "unknown key", value.Line+1)
				}
				*target, err = scalar(SystemFile, value, 1)
				if err != nil {
					return "", 0, err
				}
			}
		} else {
			r.model.ID, err = scalar(SystemFile, v, 1)
			if err != nil {
				return "", 0, err
			}
		}
		if strings.TrimSpace(r.model.ID) == "" {
			return "", 0, invalid(SystemFile, "missing model", v.Line+1)
		}
	}
	return strings.Join(lines[end+1:], ""), end + 2, nil
}
