package runners

import (
	"testing"

	"github.com/mash/kioku/prompts"
)

func defaultPrompts(t *testing.T) *prompts.Rendered {
	t.Helper()
	r, err := prompts.Load(prompts.Variables{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func loadPrompts(t *testing.T, files map[string]string) *prompts.Rendered {
	t.Helper()
	r, err := prompts.LoadWith(files, prompts.Variables{BaseURL: "https://kioku.test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
