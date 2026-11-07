package runners

import (
	"github.com/mash/kioku/internal/model"
	openairunner "github.com/mash/kioku/internal/runner/openai"
	"github.com/mash/kioku/internal/store"
	"testing"
)

func TestOpenAISelection(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := Config{Type: "openai", BaseURL: "https://kioku.test", OpenAIAPIKey: "test-key", OpenAIModel: "test-model"}
	s, err := Load(c, db, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	r, err := s.New(model.Job{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(*openairunner.Runner); !ok {
		t.Fatalf("%T", r)
	}
	for _, change := range []func(*Config){func(c *Config) { c.OpenAIAPIKey = "" }, func(c *Config) { c.OpenAIModel = "" }, func(c *Config) { c.BaseURL = "http://kioku.test" }, func(c *Config) { c.BaseURL = "https://kioku.test:8080" }} {
		bad := c
		change(&bad)
		if _, err := Load(bad, nil, nil, nil); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
