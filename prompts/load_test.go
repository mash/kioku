package prompts

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

var testVars = Variables{BaseURL: "https://kioku.test", ClientVersion: "9.8.7", MaxOutputBytes: 2468}

func writePrompt(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func loadTest(t *testing.T, dir string) *Rendered {
	t.Helper()
	r, err := Load(dir, testVars, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func defaultPrompts(t *testing.T) *Rendered {
	t.Helper()
	return loadTest(t, "")
}
func loadFiles(t *testing.T, files map[string]string) (*Rendered, error) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		writePrompt(t, dir, name, body)
	}
	return Load(dir, testVars, nil)
}
func TestDefaultsAndCopiedFiles(t *testing.T) {
	want := defaultPrompts(t)
	dir := t.TempDir()
	if got := loadTest(t, dir); !reflect.DeepEqual(got.Sources(), want.Sources()) {
		t.Fatal("missing files changed defaults")
	}
	for _, name := range []string{SystemFile, UserFile, MCPFile} {
		b, err := defaults.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		writePrompt(t, dir, name, string(b))
	}
	writePrompt(t, dir, ".ignored", "private")
	got := loadTest(t, dir)
	for i, source := range got.Sources() {
		if source.Source != "override" || source.SHA256 != want.Sources()[i].SHA256 {
			t.Fatal(source)
		}
	}
	if !strings.Contains(got.System(), "Kioku-Client-Version: 9.8.7") || !strings.Contains(got.System(), "2468 bytes") {
		t.Fatal("default variables missing")
	}
	job := JobVariables{"j", "p", "https://kioku.test/input", "https://kioku.test/output"}
	if got.User(job) != "job: j\nproject: p\ninput: https://kioku.test/input\noutput: https://kioku.test/output\n" {
		t.Fatal(got.User(job))
	}
	if strings.Contains(got.System(), job.InputURL) {
		t.Fatal("job in system")
	}
	if got.Instructions() != "" {
		t.Fatal("default instructions should be absent")
	}
}
func TestRejectInvalidPromptsWithoutEchoingContents(t *testing.T) {
	for _, tc := range []struct{ file, body, kind string }{
		{SystemFile, "---\nprivate-secret: foo\n---\nbody", "unknown key"},
		{SystemFile, "---\ntools: []\n---\nbody", "unknown key"},
		{SystemFile, "---\nmodel: {id: x, private-secret: x}\n---\nbody", "unknown key"},
		{SystemFile, "---\nmodel: [private-secret\n---\nbody", "syntax"},
		{SystemFile, "---\nmodel: 123\n---\nbody", "syntax"},
		{SystemFile, "---\nmodel: {effort: high}\n---\nbody", "missing model"},
		{SystemFile, "---\nmodel: {id: x, effort: {type: high}}\n---\nbody", "syntax"},
		{SystemFile, "---\nmodel: {id: x, effort: {}}\n---\nbody", "syntax"},
		{SystemFile, "---\nmodel: {id: x, id: y}\n---\nbody", "syntax"},
		{SystemFile, "---\nmodel: x\n", "syntax"},
		{SystemFile, "{{.privateSecret}}", "undefined variable"},
		{SystemFile, "{{.JobID}}", "per-job variable"},
		{SystemFile, "{{.InputURL}}", "per-job variable"},
		{SystemFile, "{{if .BaseURL}}private-secret{{end}}", "forbidden syntax"},
		{SystemFile, "{{range .BaseURL}}private-secret{{end}}", "forbidden syntax"},
		{SystemFile, "{{with .BaseURL}}private-secret{{end}}", "forbidden syntax"},
		{SystemFile, "{{.BaseURL | printf \"private-secret\"}}", "forbidden syntax"},
		{SystemFile, "{{printf \"private-secret\"}}", "forbidden syntax"},
		{SystemFile, "{{privateSecret}}", "forbidden syntax"},
		{SystemFile, "{{define \"private-secret\"}}x{{end}}", "forbidden syntax"},
		{SystemFile, "{{define \"prompt\"}}x{{end}}", "forbidden syntax"},
		{SystemFile, "{{template \"private-secret\"}}", "forbidden syntax"},
		{SystemFile, "{{block \"prompt\" .}}x{{end}}", "forbidden syntax"},
		{SystemFile, "{{.BaseURL.X}}", "forbidden syntax"},
		{SystemFile, "{{/* private-secret */}}", "forbidden syntax"},
		{SystemFile, "{{$x := .BaseURL}}{{$x}}", "forbidden syntax"},
		{SystemFile, "{{\"private-secret\"}}", "forbidden syntax"},
		{SystemFile, "{{( .BaseURL )}}", "forbidden syntax"},
		{SystemFile, "{{.BaseURL", "forbidden syntax"},
		{UserFile, "{{.InputURL}} private-secret", "missing URL"},
		{UserFile, "{{.OutputURL}} private-secret", "missing URL"},
		{UserFile, "{{\"{{\"}}.InputURL}} {{.OutputURL}}", "missing URL"},
		{UserFile, "---\nmodel: x\n---\n{{.InputURL}} {{.OutputURL}}", "syntax"},
		{UserFile, "\ufeff---\nmodel: x\n---\n{{.InputURL}} {{.OutputURL}}", "syntax"},
		{UserFile, "\ufeff---\r\nmodel: x\r\n---\r\n{{.InputURL}} {{.OutputURL}}", "syntax"},
		{MCPFile, "private-secret: x", "unknown key"},
		{MCPFile, "tools: {wiki_read: {inputSchema: {}}}", "unknown key"},
		{MCPFile, "tools: {wiki_read: {description: ''}}", "empty description"},
		{MCPFile, "tools: {wiki_read: {description: 3}}", "syntax"},
		{MCPFile, "tools: {wiki_read: {description: '{{.JobID}}'}}", "per-job variable"},
		{MCPFile, "instructions: '{{.InputURL}}'", "per-job variable"},
		{MCPFile, "instructions: '{{.privateSecret}}'", "undefined variable"},
		{MCPFile, "instructions: {{.BaseURL}} private-secret", "syntax"},
		{MCPFile, "instructions: [private-secret", "syntax"},
		{MCPFile, "instructions: a\ninstructions: b", "syntax"},
		{MCPFile, "instructions: a\n---\ninstructions: b", "syntax"},
		{MCPFile, "instructions: &text hi\ntools: {wiki_read: {description: *text}}", "syntax"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			_, err := loadFiles(t, map[string]string{tc.file: tc.body})
			if err == nil || !strings.Contains(err.Error(), tc.kind) {
				t.Fatalf("want %s, got %v", tc.kind, err)
			}
			if strings.Contains(err.Error(), "private-secret") || strings.Contains(err.Error(), "privateSecret") || !strings.Contains(err.Error(), "line ") {
				t.Fatal("unsafe or unlocated error", err)
			}
		})
	}
}

func TestLoadFrontmatterDelimiterWhitespace(t *testing.T) {
	for _, tc := range []struct{ name, opening, closing string }{
		{"opening spaces", "--- \n", "---\n"},
		{"opening tabs", "---\t\n", "---\n"},
		{"closing spaces", "---\n", "--- \n"},
		{"closing tabs", "---\n", "---\t\n"},
		{"mixed whitespace", "--- \t\r\n", "---\t \r\n"},
		{"BOM and whitespace", "\ufeff--- \t\r\n", "--- \t\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("system", func(t *testing.T) {
				r, err := loadFiles(t, map[string]string{SystemFile: tc.opening + "model: custom\n" + tc.closing + "body\n"})
				if err != nil {
					t.Fatal(err)
				}
				if r.Model() != (Model{ID: "custom"}) || r.System() != "body\n" {
					t.Fatalf("model = %+v, system = %q", r.Model(), r.System())
				}
			})
			t.Run("user", func(t *testing.T) {
				_, err := loadFiles(t, map[string]string{UserFile: tc.opening + "model: custom\n" + tc.closing + "{{.InputURL}} {{.OutputURL}}"})
				want := invalid(UserFile, "syntax", 1).Error()
				if err == nil || err.Error() != want {
					t.Fatalf("want %q, got %v", want, err)
				}
			})
		})
	}
}

func TestLoadEmptyFrontmatter(t *testing.T) {
	for _, front := range []string{"", " \t\r\n", "# model: x\n", "# comment\r\n\r\n  # model: x\r\n"} {
		t.Run(front, func(t *testing.T) {
			prefix := "---\n" + front + "---\n"
			r, err := loadFiles(t, map[string]string{SystemFile: prefix + "body\n"})
			if err != nil {
				t.Fatal(err)
			}
			if r.Model() != (Model{}) || r.System() != "body\n" {
				t.Fatalf("model = %+v, system = %q", r.Model(), r.System())
			}
			_, err = loadFiles(t, map[string]string{SystemFile: prefix + "{{.Unknown}}"})
			want := invalid(SystemFile, "undefined variable", strings.Count(prefix, "\n")+1).Error()
			if err == nil || err.Error() != want {
				t.Fatalf("want %q, got %v", want, err)
			}
		})
	}
}

func TestLoadStripsLeadingBOM(t *testing.T) {
	for _, tc := range []struct{ name, file, body string }{
		{"system frontmatter", SystemFile, "---\nmodel: custom\n---\nHello {{.BaseURL}}"},
		{"system CRLF", SystemFile, "---\r\nmodel: custom\r\n---\r\nHello {{.BaseURL}}"},
		{"system body", SystemFile, "Hello {{.BaseURL}}"},
		{"user", UserFile, "{{.InputURL}} {{.OutputURL}}"},
		{"mcp", MCPFile, "instructions: 'Hello {{.BaseURL}}'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePrompt(t, dir, tc.file, tc.body)
			want := loadTest(t, dir)
			writePrompt(t, dir, tc.file, "\ufeff"+tc.body)
			got := loadTest(t, dir)
			if !reflect.DeepEqual(got.Sources(), want.Sources()) {
				t.Fatal("leading BOM changed hashes")
			}

		})
	}
	dir := t.TempDir()
	writePrompt(t, dir, SystemFile, "\ufeff\ufeffbody\ufeff")
	got := loadTest(t, dir)
	writePrompt(t, dir, SystemFile, "\ufeffbody\ufeff")
	if reflect.DeepEqual(got.Sources(), loadTest(t, dir).Sources()) {
		t.Fatal("should strip only one leading BOM")
	}
}

func TestLoadRejectsEmptyRenderedBodies(t *testing.T) {
	for _, tc := range []struct {
		name, file, body string
		line             int
	}{
		{"empty system", SystemFile, "", 1},
		{"whitespace system", SystemFile, " \t\r\n\u3000", 1},
		{"frontmatter only", SystemFile, "---\nmodel: custom\n---\n", 4},
		{"empty frontmatter only", SystemFile, "---\n---\n", 3},
		{"frontmatter and whitespace", SystemFile, "---\r\nmodel: custom\r\n---\r\n \t\r\n", 4},
		{"empty system expansion", SystemFile, "{{.BaseURL}}", 1},
		{"whitespace system expansion", SystemFile, "---\nmodel: custom\n---\n{{.ClientVersion}}\n", 4},
		{"empty user", UserFile, "", 1},
		{"whitespace user", UserFile, " \t\r\n\u3000", 1},
		{"empty user expansion", UserFile, "{{.BaseURL}}", 1},
		{"whitespace user expansion", UserFile, "{{.ClientVersion}}\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePrompt(t, dir, tc.file, tc.body)
			_, err := Load(dir, Variables{ClientVersion: " \t"}, nil)
			want := invalid(tc.file, "empty body", tc.line).Error()
			if err == nil || err.Error() != want {
				t.Fatalf("want %q, got %v", want, err)
			}
		})
	}

	// Job variables are supplied later; a URL-only template is still non-empty.
	dir := t.TempDir()
	writePrompt(t, dir, UserFile, "{{.InputURL}}\n{{.OutputURL}}")
	r, err := Load(dir, Variables{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.User(JobVariables{InputURL: "input", OutputURL: "output"}); got != "input\noutput" {
		t.Fatal(got)
	}
}

func TestMCPTemplateErrorsUseValueStartLine(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		line       int
	}{
		{"escaped newlines", `instructions: "\n\n\nACTION"`, 1},
		{"literal", "# header\ninstructions: |\n  text\n  ACTION\n", 2},
		{"folded", "# header\ninstructions: >\n  first\n  second\n\n  ACTION\n", 2},
		{"tool description", "tools:\n  wiki_read:\n    description:\n      'text\n\n      ACTION'\n", 4},
		{"property description", "tools:\n  wiki_read:\n    properties:\n      path:\n        description: |\n          text\n          ACTION\n", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, action := range []struct{ text, kind string }{
				{"{{.Private}}", "undefined variable"},
				{"{{.InputURL}}", "per-job variable"},
				{"{{if .BaseURL}}", "forbidden syntax"},
			} {
				t.Run(action.kind, func(t *testing.T) {
					_, err := loadFiles(t, map[string]string{MCPFile: strings.ReplaceAll(tc.body, "ACTION", action.text)})
					want := invalid(MCPFile, action.kind, tc.line).Error()
					if err == nil || err.Error() != want {
						t.Fatalf("want %q, got %v", want, err)
					}
				})
			}
		})
	}
}

func TestMCPRejectsEmptyAndDuplicateKeys(t *testing.T) {
	for _, tc := range []struct{ body, kind string }{
		{`"": {instructions: x}`, "unknown key"},
		{"instructions: first\n\"\": {instructions: second}", "unknown key"},
		{"\"\": {instructions: first}\ninstructions: second", "unknown key"},
		{`tools: {"": {wiki_read: {description: x}}}`, "unknown key"},
		{`tools: {wiki_read: {"": {description: x}}}`, "unknown key"},
		{`tools: {wiki_read: {properties: {"": {path: {description: x}}}}}`, "unknown key"},
		{`tools: {wiki_read: {properties: {path: {"": {description: x}}}}}`, "unknown key"},
		{"instructions: first\n\"instructions\": second", "syntax"},
		{`tools: {wiki_read: {description: first, "description": second}}`, "syntax"},
		{"tools.wiki_read.description: first\ntools: {wiki_read: {description: second}}", "unknown key"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			if _, err := loadFiles(t, map[string]string{MCPFile: tc.body}); err == nil || !strings.Contains(err.Error(), tc.kind) {
				t.Fatalf("want %s, got %v", tc.kind, err)
			}
		})
	}
}

func TestTemplateActionsRequireExactSpelling(t *testing.T) {
	for _, action := range []string{
		`{{.BaseURL |}}`, `{{.BaseURL|}}`, `{{"{{" |}}`,
		`{{ .BaseURL}}`, `{{.BaseURL }}`, `{{ "{{"}}`, `{{"{{" }}}`,
		"{{\t.BaseURL}}", "{{.BaseURL\n}}",
		`{{- .BaseURL}}`, `{{.BaseURL -}}`, `{{/* comment */}}`,
		"{{`{{`}}", `{{"\x7b\x7b"}}`, `{{.BaseURL.X}}`,
		`{{.123}}`, `{{.Base-URL}}`, `{{.BaseURL`, `{{.Private}}`,
	} {
		t.Run(action, func(t *testing.T) {
			for _, file := range []string{SystemFile, UserFile, MCPFile} {
				t.Run(file, func(t *testing.T) {
					body, line := "text\n"+action, 2
					switch file {
					case SystemFile:
						body, line = "---\nmodel: custom\n---\n"+body, 5
					case UserFile:
						body += " {{.InputURL}} {{.OutputURL}}"
					case MCPFile:
						body, line = "instructions: |\n  "+strings.ReplaceAll(body, "\n", "\n  ")+"\n", 1
					}
					_, err := loadFiles(t, map[string]string{file: body})
					kind := "forbidden syntax"
					if action == "{{.Private}}" {
						kind = "undefined variable"
					}
					want := invalid(file, kind, line).Error()
					if err == nil || err.Error() != want {
						t.Fatalf("want %q, got %v", want, err)
					}
				})
			}
		})
	}
}
func TestDirectoryValidation(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "file", "private-secret")
	for _, path := range []string{filepath.Join(dir, "missing"), filepath.Join(dir, "file")} {
		if _, err := Load(path, testVars, nil); err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatal(err)
		}
	}
	for _, symlink := range []bool{false, true} {
		t.Run(strconv.FormatBool(symlink), func(t *testing.T) {
			d := t.TempDir()
			if symlink {
				if err := os.Symlink(filepath.Join(dir, "file"), filepath.Join(d, "private-secret")); err != nil {
					t.Fatal(err)
				}
			} else {
				writePrompt(t, d, "private-secret", "hidden")
			}
			if _, err := Load(d, testVars, nil); err == nil || !strings.Contains(err.Error(), "unknown file") || strings.Contains(err.Error(), "private-secret") {
				t.Fatal(err)
			}
		})
	}
	for _, kind := range []string{"directory", "dangling"} {
		d := t.TempDir()
		path := filepath.Join(d, SystemFile)
		if kind == "directory" {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Symlink(filepath.Join(d, "absent"), path); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := Load(d, testVars, nil); err == nil {
			t.Fatal("unreadable prompt accepted")
		}
	}
}
func TestTemplatesMCPMergeAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, SystemFile, "---\nmodel: {id: custom, effort: high, speed: fast, inference_geo: us}\n---\n{{.BaseURL}} {{.ClientVersion}} {{.MaxOutputBytes}} {{\"{{\"}} $KIOKU_JOB_TOKEN")
	writePrompt(t, dir, UserFile, "custom {{.JobID}} {{.ProjectID}} {{.InputURL}} {{.OutputURL}} {{.BaseURL}}")
	writePrompt(t, dir, MCPFile, "instructions: 'Welcome {{.BaseURL}}'\ntools:\n  wiki_read:\n    description: '{{.BaseURL}} wiki'\n    properties:\n      project:\n        description: |\n          Select {{.BaseURL}}\n")
	r := loadTest(t, dir)
	if r.System() != "https://kioku.test 9.8.7 2468 {{ $KIOKU_JOB_TOKEN" {
		t.Fatal(r.System())
	}
	if r.Model() != (Model{ID: "custom", Effort: "high", Speed: "fast", InferenceGeo: "us"}) {
		t.Fatal(r.Model())
	}
	if r.User(JobVariables{"j", "p", "in", "out"}) != "custom j p in out https://kioku.test" {
		t.Fatal("user variables")
	}
	if r.Instructions() != "Welcome https://kioku.test" {
		t.Fatal(r.Instructions())
	}
	base := defaultPrompts(t).MCP()
	got := r.MCP()
	tool := got.Tools["wiki_read"]
	if tool.Description != "https://kioku.test wiki" || tool.Properties["project"].Description != "Select https://kioku.test\n" {
		t.Fatal(tool)
	}
	tool.Description = base.Tools["wiki_read"].Description
	delete(tool.Properties, "project")
	got.Tools["wiki_read"] = tool
	got.Instructions = base.Instructions
	if hash(got) != hash(base) {
		t.Fatal("other descriptions changed")
	}
	writePrompt(t, dir, SystemFile, "changed")
	if r.System() == "changed" {
		t.Fatal("snapshot changed")
	}
	if r.MCP().Tools["wiki_read"].Description != "https://kioku.test wiki" {
		t.Fatal("caller changed snapshot")
	}
}
func TestLog(t *testing.T) {
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	defer slog.SetDefault(old)
	dir := t.TempDir()
	writePrompt(t, dir, SystemFile, "---\nmodel: {id: private-secret, effort: high, speed: fast}\n---\nprivate-secret")
	r := loadTest(t, dir)
	r.Log()
	if strings.Count(log.String(), "sha256=") != 3 || strings.Count(log.String(), "source=override") != 1 || strings.Count(log.String(), "source=default") != 2 || strings.Contains(log.String(), "private-secret") {
		t.Fatal(log.String())
	}
	if r.Model().ID == "" {
		t.Fatal("model lost")
	}
	writePrompt(t, dir, SystemFile, "no frontmatter")
	if r := loadTest(t, dir); r.Model() != (Model{}) {
		t.Fatal("omitted model should not inherit default")
	}
}
func TestHashTracksEffectiveChanges(t *testing.T) {
	original := defaultPrompts(t)
	for _, name := range []string{SystemFile, UserFile, MCPFile} {
		dir := t.TempDir()
		body, _ := defaults.ReadFile(name)
		if name == MCPFile {
			body = []byte("instructions: extra")
		} else {
			body = append(body, []byte("\nextra")...)
		}
		writePrompt(t, dir, name, string(body))
		r := loadTest(t, dir)
		for i, s := range r.Sources() {
			if (s.SHA256 == original.Sources()[i].SHA256) == (s.File == name) {
				t.Fatal("hash mismatch", s)
			}
		}
	}
}

func TestUserHashDistinguishesFieldsFromLiteralBraces(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, UserFile, `{{.JobID}} {{.InputURL}} {{.OutputURL}}`)
	field := loadTest(t, dir)
	writePrompt(t, dir, UserFile, `{{"{{"}}.JobID}} {{.InputURL}} {{.OutputURL}}`)
	literal := loadTest(t, dir)
	if field.Sources()[1].SHA256 == literal.Sources()[1].SHA256 {
		t.Fatal("different template behavior has the same hash")
	}
}
func TestModelValuesArePlainStrings(t *testing.T) {
	for _, front := range []string{
		`model: "{{.BaseURL}}"`,
		`model: {id: "{{.BaseURL}}", effort: "{{.JobID}}", speed: "{{if .BaseURL}}", inference_geo: "{{.Unknown}}"}`,
	} {
		r, err := loadFiles(t, map[string]string{SystemFile: "---\n" + front + "\n---\nbody"})
		if err != nil {
			t.Fatal(err)
		}
		if r.Model().ID != "{{.BaseURL}}" {
			t.Fatal(r.Model())
		}
		if strings.Contains(front, "effort") && r.Model() != (Model{"{{.BaseURL}}", "{{.JobID}}", "{{if .BaseURL}}", "{{.Unknown}}"}) {
			t.Fatal(r.Model())
		}
	}
	for _, value := range []string{"[]", "null", "true", "{id: x, effort: 1}", "{id: x, effort: []}", "{id: x, speed: {}}", "{id: x, inference_geo: []}"} {
		_, err := loadFiles(t, map[string]string{SystemFile: "---\nmodel: " + value + "\n---\nbody"})
		if err == nil || !strings.Contains(err.Error(), "syntax") {
			t.Fatal(value, err)
		}
	}
}
