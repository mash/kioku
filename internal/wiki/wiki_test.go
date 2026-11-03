package wiki

import (
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
)

func TestAssemblePreservesSourcesAndPathsRemainRestricted(t *testing.T) {
	for _, p := range []string{"../secret", "wiki/../../secret.md", "/wiki/a.md", "wiki/a\\b.md", "wiki/a:foo.md", "wiki//a.md", "wiki/../foo.md", "maps/evil.html"} {
		if ValidFile(p) {
			t.Errorf("accepted %s", p)
		}
	}
	files, err := Assemble([]Input{{Source: model.Source{ID: "src1", Title: "Evidence", ProjectID: "p1"}, Text: "A decision"}}, map[string]string{"wiki/index.md": "# Overview\n[Evidence](source-src1.md)", "wiki/decisions.md": "# Decisions\n[Evidence](source-src1.md)"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files["wiki/source-src1.md"], "A decision") || !strings.Contains(files["wiki/decisions.md"], "Source snapshots") {
		t.Fatal(files)
	}
}
