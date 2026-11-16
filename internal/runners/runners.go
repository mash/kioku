// Package runners configures and selects generation runners.
package runners

import (
	"context"
	"slices"

	"github.com/mash/kioku/prompts"

	execrunner "github.com/mash/kioku/internal/runner/exec"

	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/runner/claudemanaged"
)

type Config struct {
	Prompts         *prompts.Rendered
	Type            string
	Command         []string
	DataDir         string
	AnthropicAPIKey string
	MAAgentID       string
	MAAgentVersion  int64
	MAEnvironmentID string
	MABaseURL       string
	ManagedAgents   claudemanaged.FileClient
	BaseURL         string
}

type Set struct {
	config     Config
	execRunner *execrunner.Runner
}

func Load(c Config, onExit func(string)) (*Set, error) {
	c.Command = slices.Clone(c.Command)
	s := &Set{config: c}
	if err := s.prepareRunner(onExit); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Set) Close(ctx context.Context) error { return s.execRunner.Close(ctx) }

// New returns the configured runner.
func (s *Set) New() (runner.Runner, error) {
	switch s.config.Type {
	case "exec":
		return s.execRunner, nil
	}
	if s.config.ManagedAgents == nil {
		return nil, runner.ErrNotConfigured
	}
	return s.claudeRunner(), nil
}

func (s *Set) claudeRunner() runner.Runner {
	c := s.config
	return claudemanaged.New(c.ManagedAgents, c.MAAgentID, c.MAAgentVersion, c.MAEnvironmentID, c.Prompts.FileSystem(), c.Prompts.Model())
}
