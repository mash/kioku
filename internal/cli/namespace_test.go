package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNamespaceStorage(t *testing.T) {
	a := &App{Dir: t.TempDir(), Config: Config{Project: "project-id", Namespace: "acme/platform"}}
	for i := 0; i < 2; i++ {
		if err := a.ensureProject(); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"", "inbox", "wiki", "maps", ".kioku"} {
		info, err := os.Lstat(filepath.Join(a.Dir, name))
		if err != nil || !info.IsDir() {
			t.Fatal("expected real directory", name, err)
		}
	}

}

func TestNamespaceStorageRejectsUnsafePaths(t *testing.T) {
	for _, namespace := range []string{"../escape", "acme/../escape", "acme/日本語", "acme/a.b", "ACME/project", "acme/a_b", "acme/a--b", "acme/-bad"} {
		a := &App{Dir: t.TempDir(), Config: Config{Project: "project-id", Namespace: namespace}}
		if err := a.ensureProject(); err == nil {
			t.Fatal("accepted", namespace)
		}
	}
	a := &App{Dir: t.TempDir(), Config: Config{Project: "project-id", Namespace: "acme/platform"}}
	outside := t.TempDir()
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(a.Dir, ".kioku")); err != nil {
		t.Fatal(err)
	}
	if err := a.ensureProject(); err == nil {
		t.Fatal("organization symlink accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("wrote outside config", entries, err)
	}
}
