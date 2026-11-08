package runners

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/prompts"
)

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
		if _, err := Load(c, nil); err == nil {
			t.Fatal(mask)
		}
	}
}

func TestPromptValidationForSelectedRunner(t *testing.T) {
	for _, count := range []int{100000, 100001} {
		rendered := loadPrompts(t, map[string]string{prompts.SystemFile: strings.Repeat("あ", count)})
		for _, kind := range []string{"", "claudemanaged", "exec", "openai"} {
			c := execConfig(t)
			c.Type, c.Prompts = kind, rendered
			c.MAAgentID, c.MAEnvironmentID, c.AnthropicAPIKey = "agent", "env", "key"
			c.BaseURL = "https://kioku.test"
			c.OpenAIAPIKey, c.OpenAIModel = "key", "model"
			set, err := Load(c, nil)
			want := ""
			if kind == "exec" {
				want = "prompts: wiki-agent.system.md: missing model (line 1)"
			}
			if (kind == "" || kind == "claudemanaged") && count > 100000 {
				want = "prompts: wiki-agent.system.md: too long (line 1)"
			}
			if want != "" {
				if err == nil || err.Error() != want {
					t.Fatalf("%s at %d: want %q, got %v", kind, count, want, err)
				}
			} else {
				if err != nil {
					t.Fatal(kind, count, err)
				}
				set.Close(t.Context())
			}
		}
	}
}

func TestExecModelOptionsWarnOnce(t *testing.T) {
	for _, fields := range []string{"", ", effort: high", ", speed: fast", ", inference_geo: us", ", effort: high, speed: fast, inference_geo: us"} {
		t.Run(fields, func(t *testing.T) {
			c := execConfig(t)
			c.Prompts = loadPrompts(t, map[string]string{prompts.SystemFile: "---\nmodel: {id: private-secret" + fields + "}\n---\nprivate-secret"})
			var log bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
			defer slog.SetDefault(old)
			set, err := Load(c, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer set.Close(t.Context())
			for range 2 {
				if _, err := set.New(model.Job{}); err != nil {
					t.Fatal(err)
				}
			}
			want := 0
			if fields != "" {
				want = 1
			}
			if strings.Count(log.String(), "level=WARN") != want || strings.Contains(log.String(), "private-secret") {
				t.Fatal(log.String())
			}
		})
	}
}
