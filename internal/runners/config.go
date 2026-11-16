package runners

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"unicode/utf8"

	"github.com/mash/kioku/internal/runner/claudemanaged"
	execrunner "github.com/mash/kioku/internal/runner/exec"
	"github.com/mash/kioku/prompts"
)

func prepareManaged(c *Config) error {
	count := 0
	for _, v := range []string{c.MAAgentID, c.MAEnvironmentID, c.AnthropicAPIKey} {
		if v != "" {
			count++
		}
	}
	if count != 0 && count != 3 {
		return errors.New("ANTHROPIC_API_KEY, KIOKU_MA_AGENT_ID and KIOKU_MA_ENVIRONMENT_ID must be configured together")
	}
	if count == 3 {
		if !validBaseOrigin(c.BaseURL) {
			return errors.New("managed generation requires a valid KIOKU_BASE_URL origin")
		}
		if c.ManagedAgents == nil {
			c.ManagedAgents = claudemanaged.NewAPI(c.MABaseURL, c.AnthropicAPIKey, nil)
		}
	}
	return nil
}

// Load validates the runner configuration and builds the selected runner.
func Load(c Config, onExit func(string)) (*Set, error) {
	if err := prepareManaged(&c); err != nil {
		return nil, err
	}
	kind := c.Type
	if kind == "" && c.ManagedAgents != nil {
		kind = "claudemanaged"
	}
	if kind != "" && c.Prompts == nil {
		return nil, errors.New("prompts are not configured")
	}
	switch kind {
	case "":
		return &Set{}, nil
	case "claudemanaged":
		if c.ManagedAgents == nil {
			return nil, errors.New("KIOKU_RUNNER=claudemanaged requires Managed Agents configuration")
		}
		if utf8.RuneCountInString(c.Prompts.FileSystem()) > 100000 {
			return nil, fmt.Errorf("prompts: %s: too long (line 1)", prompts.SystemFile)
		}
		return &Set{Runner: claudemanaged.New(c.ManagedAgents, c.MAAgentID, c.MAAgentVersion, c.MAEnvironmentID, c.Prompts.FileSystem(), c.Prompts.Model())}, nil
	case "exec":
	default:
		return nil, errors.New("unknown KIOKU_RUNNER")
	}
	model := c.Prompts.Model()
	if model.ID == "" {
		return nil, fmt.Errorf("prompts: %s: missing model (line 1)", prompts.SystemFile)
	}
	if model.Effort != "" || model.Speed != "" || model.InferenceGeo != "" {
		slog.Warn("exec runner ignores model effort, speed and inference_geo", "file", prompts.SystemFile)
	}
	command := c.Command
	if len(command) == 0 || !filepath.IsAbs(command[0]) {
		return nil, errors.New("KIOKU_RUNNER_COMMAND must be a nonempty JSON argv array with an absolute executable path")
	}
	if _, err := exec.LookPath(command[0]); err != nil {
		return nil, errors.New("KIOKU_RUNNER_COMMAND executable is not runnable")
	}
	if !validBaseOrigin(c.BaseURL) {
		return nil, errors.New("exec generation requires a valid KIOKU_BASE_URL origin")
	}
	data := c.DataDir
	if data == "" {
		data = "./data"
	}
	root, err := filepath.Abs(filepath.Join(data, "exec-runs"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	r := execrunner.New(execrunner.Config{Root: root, Command: command, Model: model, Prompt: c.Prompts.System()}, onExit)
	return &Set{Runner: r, exec: r}, nil
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
