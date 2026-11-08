package runners

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mash/kioku/prompts"
)

func defaultPrompts(t *testing.T) *prompts.Rendered {
	t.Helper()
	r, err := prompts.Load("", prompts.Variables{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func loadPrompts(t *testing.T, files map[string]string) *prompts.Rendered {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := prompts.Load(dir, prompts.Variables{BaseURL: "https://kioku.test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
