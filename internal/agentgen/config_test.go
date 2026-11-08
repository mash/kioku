package agentgen

import (
	"os"
	"strings"
	"testing"

	"github.com/mash/kioku/prompts"
	"gopkg.in/yaml.v3"
)

func TestAgentConfigLimitsCapabilitiesAndMatchesOutputRules(t *testing.T) {
	rendered, err := prompts.Load("", prompts.Variables{ClientVersion: "test-version", MaxOutputBytes: 128 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	system := rendered.System()
	if rendered.Model().ID != "claude-opus-5-5" {
		t.Fatal(rendered.Model())
	}
	for _, rule := range strings.Split(prompts.OutputRules, "\n") {
		if !strings.Contains(system, rule) {
			t.Errorf("prompt lacks %q", rule)
		}
	}
	for _, text := range []string{"untrusted data", "up to 30 times", "10 seconds", "tarfile", "Authorization: Bearer $KIOKU_JOB_TOKEN", "inbox/", "source_url", "100 entries", "Kioku-Client-Version: test-version", "134217728 bytes", "JOB.md holds the job metadata"} {
		if !strings.Contains(system, text) {
			t.Error("prompt missing", text)
		}
	}
}
func TestEnvironmentDeniesOutboundNetwork(t *testing.T) {
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
	if env.Name != "kioku-wiki" || env.Config.Type != "cloud" || n.Type != "limited" || n.Hosts == nil || len(n.Hosts) != 0 || n.Packages == nil || *n.Packages || n.MCP == nil || *n.MCP {
		t.Fatal(env)
	}
}
