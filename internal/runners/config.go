package runners

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mash/kioku/internal/managedagents"
	execrunner "github.com/mash/kioku/internal/runner/exec"
	openairunner "github.com/mash/kioku/internal/runner/openai"
	"github.com/mash/kioku/prompts"
	"gopkg.in/yaml.v3"
)

func (s *Set) prepareManaged() error {
	c := &s.config
	count := 0
	for _, v := range []string{c.MAAgentID, c.MAEnvironmentID, c.AnthropicAPIKey} {
		if v != "" {
			count++
		}
	}
	if count != 0 && count != 3 {
		return errors.New("ANTHROPIC_API_KEY, KIOKU_MA_AGENT_ID and KIOKU_MA_ENVIRONMENT_ID must be configured together")
	}
	if c.MAAgentVersion < 0 || c.AgentMaxSessions < 1 || c.AgentJobTimeout <= 0 || c.AgentMaxSessionCents < 50 || c.AgentMaxSessionCents > 1_000_000 {
		return errors.New("invalid managed agent limits")
	}
	if count == 3 {
		if !validBaseOrigin(c.BaseURL) {
			return errors.New("managed generation requires a valid KIOKU_BASE_URL origin")
		}
		if c.ManagedAgents == nil {
			c.ManagedAgents = managedagents.New(c.MABaseURL, c.AnthropicAPIKey, nil)
		}
	}
	return nil
}

func (s *Set) prepareRunner(onExit func(string, string)) error {
	if err := s.prepareManaged(); err != nil {
		return err
	}
	data := s.config.DataDir
	if data == "" {
		data = "./data"
	}
	root, err := filepath.Abs(data)
	if err != nil {
		return err
	}
	s.config.DataDir = root
	root = filepath.Join(root, "exec-runs")
	config := execrunner.Config{Root: root}
	switch s.config.Type {
	case "":
	case "managed":
		if s.config.ManagedAgents == nil {
			return errors.New("KIOKU_RUNNER=managed requires Managed Agents configuration")
		}
	case "openai":
		if strings.TrimSpace(s.config.OpenAIAPIKey) == "" || strings.TrimSpace(s.config.OpenAIModel) == "" {
			return errors.New("KIOKU_RUNNER=openai requires OPENAI_API_KEY and KIOKU_OPENAI_MODEL")
		}
		u, err := url.Parse(s.config.BaseURL)
		if err != nil || !validBaseOrigin(s.config.BaseURL) || u.Scheme != "https" || (u.Port() != "" && u.Port() != "443" && u.Port() != "8443") {
			return errors.New("OpenAI generation requires an HTTPS KIOKU_BASE_URL on port 443 or 8443")
		}
		_, prompt, err := prompts.SplitWikiDefinition()
		if err != nil {
			return err
		}
		s.openaiRunner = openairunner.New(openairunner.Config{APIKey: s.config.OpenAIAPIKey, Model: s.config.OpenAIModel, Prompt: strings.TrimSpace(prompt)})
	case "exec":
		command := s.config.Command
		if len(command) == 0 || !filepath.IsAbs(command[0]) {
			return errors.New("KIOKU_RUNNER_COMMAND must be a nonempty JSON argv array with an absolute executable path")
		}
		if _, err := exec.LookPath(command[0]); err != nil {
			return errors.New("KIOKU_RUNNER_COMMAND executable is not runnable")
		}
		if !validBaseOrigin(s.config.BaseURL) {
			return errors.New("exec generation requires a valid KIOKU_BASE_URL origin")
		}
		frontmatter, prompt, err := prompts.SplitWikiDefinition()
		if err != nil {
			return err
		}
		var definition struct {
			Model string `yaml:"model"`
		}
		if err := yaml.Unmarshal([]byte(frontmatter), &definition); err != nil || strings.TrimSpace(definition.Model) == "" {
			return errors.New("invalid runner model")
		}
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
		config = execrunner.Config{Root: root, Command: command, Model: definition.Model, Prompt: strings.TrimSpace(prompt)}
	default:
		return errors.New("unknown KIOKU_RUNNER")
	}
	s.execRunner = execrunner.New(config, onExit)
	return nil
}

func validBaseOrigin(base string) bool {
	u, err := url.Parse(base)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func ParseCommand(runner, value string) ([]string, error) {
	if runner != "exec" {
		return nil, nil
	}
	invalid := errors.New("KIOKU_RUNNER_COMMAND must be a JSON argv array")
	var values []*string
	if err := json.Unmarshal([]byte(value), &values); err != nil || values == nil {
		return nil, invalid
	}
	command := make([]string, len(values))
	for i, value := range values {
		if value == nil {
			return nil, invalid
		}
		command[i] = *value
	}
	return command, nil
}
