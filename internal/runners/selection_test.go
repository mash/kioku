package runners

import (
	"fmt"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
)

func TestRunnerStartupSelection(t *testing.T) {
	for _, runner := range []string{"", "managed"} {
		for mask := 0; mask < 8; mask++ {
			t.Run(fmt.Sprintf("%s/%d", runner, mask), func(t *testing.T) {
				c := Config{Type: runner, BaseURL: "https://kioku.test"}
				if mask&1 != 0 {
					c.AnthropicAPIKey = "key"
				}
				if mask&2 != 0 {
					c.MAAgentID = "agent"
				}
				if mask&4 != 0 {
					c.MAEnvironmentID = "environment"
				}
				s, err := Load(c, nil, nil, nil)
				wantOK := mask == 7 || runner == "" && mask == 0
				if (err == nil) != wantOK {
					t.Fatal(err)
				}
				if err == nil && (s.config.ManagedAgents != nil) != (mask == 7) {
					t.Fatal("incorrect runner selection")
				}
			})
		}
	}
	for _, runner := range []string{"exec", "invalid"} {
		if _, err := Load(Config{Type: runner}, nil, nil, nil); err == nil {
			t.Fatal(err)
		}
	}
	for _, change := range []func(*Config){func(c *Config) { c.MAAgentVersion = -1 }, func(c *Config) { c.BaseURL = "https://kioku.test/path" }, func(c *Config) { c.AgentMaxSessionCents = 49 }} {
		c := Config{Type: "managed", AnthropicAPIKey: "key", MAAgentID: "agent", MAEnvironmentID: "environment", BaseURL: "https://kioku.test"}
		change(&c)
		if _, err := Load(c, nil, nil, nil); err == nil {
			t.Fatal("invalid managed settings accepted")
		}
	}
}

func TestBYOKIgnoresRunnerSelection(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(st *model.State) error {
		st.Users["user"] = model.User{ID: "user", Anthropic: &model.AnthropicConnection{ID: "connection", AgentID: "agent", EnvironmentID: "env"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	g, err := Load(Config{}, db, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, selection := range []string{"", "managed", "exec", "openai", "invalid"} {
		// Saved BYOK jobs must not depend on the operator's current selection.
		g.config.Type = selection
		r, err := g.New(model.Job{UserID: "user", AnthropicConnectionID: "connection"})
		if err != nil || r == nil {
			t.Fatal(selection, err)
		}
	}
}
