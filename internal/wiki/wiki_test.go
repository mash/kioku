package wiki

import (
	"strings"
	"testing"
)

func TestValidFileAcceptsOnlyWikiMarkdown(t *testing.T) {
	for _, p := range []string{"../secret", "wiki/../../secret.md", "/wiki/a.md", "wiki/a\\b.md", "wiki/a:foo.md", "wiki//a.md", "wiki/../foo.md", "maps/graph.json", "maps/graph.mmd", "wiki/view.html"} {
		if ValidFile(p) {
			t.Errorf("accepted %s", p)
		}
	}
	if !ValidFile("wiki/index.md") {
		t.Fatal("Wiki entry page rejected")
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
