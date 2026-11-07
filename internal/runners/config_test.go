package runners

import "testing"

func TestManagedConfigRequiresCompleteSettings(t *testing.T) {
	for mask := 1; mask < 7; mask++ {
		c := Config{}
		if mask&1 != 0 {
			c.MAAgentID = "agent"
		}
		if mask&2 != 0 {
			c.MAEnvironmentID = "env"
		}
		if mask&4 != 0 {
			c.AnthropicAPIKey = "key"
		}
		if _, err := Load(c, nil, nil, nil); err == nil {
			t.Fatal(mask)
		}
	}
}
