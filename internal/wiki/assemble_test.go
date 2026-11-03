package wiki

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestAssembleGolden(t *testing.T) {
	inputs := []Input{{Source: model.Source{ID: "src1", ProjectID: "p1", Title: "Evidence"}, Text: "A decision"}}
	files, err := Assemble(inputs, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(files, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/assemble.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got)+"\n" != string(want) {
		t.Fatalf("output differs from golden: %s", got)
	}
}

func TestValidatePages(t *testing.T) {
	tests := []struct {
		name  string
		pages map[string]string
		max   int
		valid bool
	}{
		{"valid", map[string]string{"wiki/index.md": "# Index", "wiki/decisions.md": "# Decisions"}, 300, true},
		{"missing-index", map[string]string{"wiki/a.md": "x"}, 300, false},
		{"empty", nil, 300, false},
		{"too-many", map[string]string{"wiki/index.md": "x", "wiki/a.md": "x"}, 1, false},
	}
	for _, name := range []string{"../x.md", "wiki/../x.md", "/wiki/a.md", "wiki//a.md", "maps/graph.json", "wiki/source-x.md", "wiki/inbox.md", "wiki/inbox/one.md", "wiki/inbox-extra.md"} {
		tests = append(tests, struct {
			name  string
			pages map[string]string
			max   int
			valid bool
		}{name, map[string]string{"wiki/index.md": "x", name: "x"}, 300, false})
	}
	tests = append(tests, struct {
		name  string
		pages map[string]string
		max   int
		valid bool
	}{"oversize", map[string]string{"wiki/index.md": strings.Repeat("x", (256<<10)+1)}, 300, false})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidatePages(tt.pages, PageLimits{tt.max}); (err == nil) != tt.valid {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestAssembleAgentProvenance(t *testing.T) {
	files, err := Assemble([]Input{{Source: model.Source{ID: "src1", ProjectID: "p1", Title: "Evidence"}, Text: "A decision"}}, map[string]string{"wiki/index.md": "# Overview\n[Evidence](source-src1.md)"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files["wiki/index.md"], "## Source snapshots") || !strings.Contains(files["wiki/source-src1.md"], "A decision") || !strings.Contains(files["maps/graph.json"], `"to": "wiki/source-src1.md"`) {
		t.Fatal(files)
	}
}

func TestAssembleReplacesTrailingSnapshots(t *testing.T) {
	files, err := Assemble(nil, map[string]string{"wiki/index.md": "# Index\n\n## Source snapshots\n\nobsolete-source"})
	if err != nil {
		t.Fatal(err)
	}
	body := files["wiki/index.md"]
	if strings.Count(body, "## Source snapshots") != 1 || strings.Contains(body, "obsolete-source") {
		t.Fatalf("stale provenance remains: %q", body)
	}
}
