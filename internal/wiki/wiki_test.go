package wiki

import (
	"testing"
)

func TestAssemblePreservesSourcesAndPathsRemainRestricted(t *testing.T) {
	for _, p := range []string{"../secret", "wiki/../../secret.md", "/wiki/a.md", "wiki/a\\b.md", "wiki/a:foo.md", "wiki//a.md", "wiki/../foo.md", "maps/evil.html"} {
		if ValidFile(p) {
			t.Errorf("accepted %s", p)
		}
	}
}
