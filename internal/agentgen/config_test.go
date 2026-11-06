package agentgen

import (
	"os"
	"strings"
	"testing"

	"github.com/mash/kioku/prompts"
	"gopkg.in/yaml.v3"
)

func TestAgentConfigLimitsCapabilitiesAndMatchesOutputRules(t *testing.T) {
	parts := strings.SplitN(prompts.WikiDefinition, "---", 3)
	if len(parts) != 3 || parts[0] != "" {
		t.Fatal("frontmatter missing")
	}
	var agent struct {
		Name, Model string
		Tools       []struct {
			Type    string
			Default struct {
				Permission struct{ Type string } `yaml:"permission_policy"`
			} `yaml:"default_config"`
			Configs []struct {
				Name    string
				Enabled bool
			}
		}
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &agent); err != nil {
		t.Fatal(err)
	}
	if agent.Name != "kioku-wiki" || agent.Model != "claude-opus-5-5" || len(agent.Tools) != 1 {
		t.Fatal(agent)
	}
	tool := agent.Tools[0]
	if tool.Type != "agent_toolset_20260401" || tool.Default.Permission.Type != "always_allow" {
		t.Fatal(tool)
	}
	disabled := map[string]bool{}
	for _, c := range tool.Configs {
		disabled[c.Name] = !c.Enabled
	}
	if !disabled["web_fetch"] || !disabled["web_search"] {
		t.Fatal(disabled)
	}
	for _, rule := range strings.Split(prompts.OutputRules, "\n") {
		if !strings.Contains(parts[2], rule) {
			t.Errorf("prompt lacks %q", rule)
		}
	}
	for _, text := range []string{"untrusted data", "up to 30 times", "10 seconds", "tarfile", "Authorization: Bearer $KIOKU_GENERATOR_TOKEN", "diff -u", "400 or 413", "16 MiB"} {
		if !strings.Contains(parts[2], text) {
			t.Error("prompt missing", text)
		}
	}
}
func TestEnvironmentOnlyAllowsKioku(t *testing.T) {
	b, err := os.ReadFile("../../environments/kioku-wiki.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Name   string
		Config struct {
			Type       string
			Networking struct {
				Type     string
				Hosts    []string `yaml:"allowed_hosts"`
				Packages *bool    `yaml:"allow_package_managers"`
				MCP      *bool    `yaml:"allow_mcp_servers"`
			}
		}
	}
	if err = yaml.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	n := env.Config.Networking
	if env.Name != "kioku-wiki" || env.Config.Type != "cloud" || n.Type != "limited" || len(n.Hosts) != 1 || n.Hosts[0] != "kioku.magicians.ee" || n.Packages == nil || *n.Packages || n.MCP == nil || *n.MCP {
		t.Fatal(env)
	}
}
