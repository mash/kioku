package wiki

import (
	"strings"
	"testing"
)

func TestAssemblePreservesPages(t *testing.T) {
	pages := map[string]string{"wiki/index.md": "# Overview\n[Evidence](source-evidence.md)", "wiki/source-evidence.md": "---\nsource_url: https://example.com/a\n---\nA decision"}
	files, err := Assemble(pages)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range pages {
		if files[name] != body {
			t.Fatal("modified page", name)
		}
	}
	if len(files) != len(pages)+2 || !strings.Contains(files["maps/graph.json"], `"to": "wiki/source-evidence.md"`) {
		t.Fatal(files)
	}
}
func TestValidatePages(t *testing.T) {
	for _, name := range []string{"../x.md", "wiki/a_b.md", "wiki/a/b.md", "maps/graph.json", "wiki/UP.md"} {
		if ValidatePages(map[string]string{"wiki/index.md": "", name: ""}) == nil {
			t.Fatal(name)
		}
	}
	if ValidatePages(map[string]string{"wiki/a.md": ""}) == nil {
		t.Fatal("missing index")
	}
	if err := ValidatePages(map[string]string{"wiki/index.md": strings.Repeat("x", 1<<20), "wiki/source-a.md": "", "wiki/inbox.md": ""}); err != nil {
		t.Fatal(err)
	}
}
