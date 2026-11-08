package runners

import (
	"fmt"
	"testing"
)

func TestRunnerStartupSelection(t *testing.T) {
	for _, runner := range []string{"", "claudemanaged"} {
		for mask := 0; mask < 8; mask++ {
			t.Run(fmt.Sprintf("%s/%d", runner, mask), func(t *testing.T) {
				c := Config{Prompts: defaultPrompts(t), Type: runner, BaseURL: "https://kioku.test"}
				if mask&1 != 0 {
					c.AnthropicAPIKey = "key"
				}
				if mask&2 != 0 {
					c.MAAgentID = "agent"
				}
				if mask&4 != 0 {
					c.MAEnvironmentID = "environment"
				}
				s, err := Load(c, nil)
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
		if _, err := Load(Config{Prompts: defaultPrompts(t), Type: runner}, nil); err == nil {
			t.Fatal(err)
		}
	}
	for _, change := range []func(*Config){func(c *Config) { c.MAAgentVersion = -1 }, func(c *Config) { c.BaseURL = "https://kioku.test/path" }, func(c *Config) { c.AgentMaxSessionCents = 49 }} {
		c := Config{Prompts: defaultPrompts(t), Type: "claudemanaged", AnthropicAPIKey: "key", MAAgentID: "agent", MAEnvironmentID: "environment", BaseURL: "https://kioku.test"}
		change(&c)
		if _, err := Load(c, nil); err == nil {
			t.Fatal("invalid claudemanaged settings accepted")
		}
	}
}

func TestLegacyManagedRunnerRejected(t *testing.T) {
	for _, configured := range []bool{false, true} {
		c := Config{Prompts: defaultPrompts(t), Type: "managed", BaseURL: "https://kioku.test"}
		if configured {
			c.AnthropicAPIKey = "key"
			c.MAAgentID = "agent"
			c.MAEnvironmentID = "environment"
		}
		if _, err := Load(c, nil); err == nil || err.Error() != "unknown KIOKU_RUNNER" {
			t.Fatalf("configured=%t: expected unknown runner error, got %v", configured, err)
		}
	}
}
