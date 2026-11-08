// Package runners configures and selects generation runners.
package runners

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/mash/kioku/prompts"

	execrunner "github.com/mash/kioku/internal/runner/exec"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/runner/claudemanaged"
	openairunner "github.com/mash/kioku/internal/runner/openai"
)

type Config struct {
	Prompts              *prompts.Rendered
	Type                 string
	OpenAIAPIKey         string
	OpenAIModel          string
	Command              []string
	DataDir              string
	AnthropicAPIKey      string
	MAAgentID            string
	MAAgentVersion       int64
	MAEnvironmentID      string
	MABaseURL            string
	ManagedAgents        claudemanaged.Client
	BaseURL              string
	AgentMaxSessions     int
	AgentJobTimeout      time.Duration
	AgentMaxSessionCents int64
}

type Set struct {
	config       Config
	execRunner   *execrunner.Runner
	openaiRunner *openairunner.FileRunner
}

func Load(c Config, onExit func(string, string)) (*Set, error) {
	if c.AgentMaxSessions == 0 {
		c.AgentMaxSessions = 4
	}
	if c.AgentJobTimeout == 0 {
		c.AgentJobTimeout = time.Hour
	}
	if c.AgentMaxSessionCents == 0 {
		c.AgentMaxSessionCents = 1000
	}
	c.Command = slices.Clone(c.Command)
	s := &Set{config: c}
	if err := s.prepareRunner(onExit); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Set) Close(ctx context.Context) error { return s.execRunner.Close(ctx) }

// Retired jobs must never execute with the server's credentials.
func (s *Set) New(job model.Job) (runner.Runner, error) {
	if job.AnthropicConnectionID != "" {
		return nil, errors.New("BYOK has been removed; this job cannot run")
	}
	if job.Run != nil {
		switch job.Run.Ref.Resources["provider"] {
		case "openai":
			if s.openaiRunner == nil {
				return nil, errors.New("OpenAI runner unavailable")
			}
			return s.openaiRunner, nil
		case "claudemanaged":
			if _, ok := s.config.ManagedAgents.(claudemanaged.FileClient); !ok {
				return nil, errors.New("Claude file runner unavailable")
			}
			return s.claudeRunner(), nil
		}
	}
	if s.config.Type == "openai" {
		if job.Run != nil && job.Run.Ref.ID != "" {
			return nil, errors.New("cannot use OpenAI credentials for an existing non-OpenAI run")
		}
		return s.openaiRunner, nil
	}
	if s.config.Type == "exec" {
		return s.execRunner, nil
	}
	if s.config.ManagedAgents == nil {
		return nil, runner.ErrNotConfigured
	}
	return s.claudeRunner(), nil
}

func (s *Set) claudeRunner() runner.Runner {
	c := s.config
	r := claudemanaged.New(func() (claudemanaged.Client, error) { return c.ManagedAgents, nil },
		c.MAAgentID, c.MAAgentVersion, c.MAEnvironmentID, c.Prompts.System(), c.Prompts.Model())
	// The production SDK client always supports file transport. The narrow
	// Client interface also supports injected legacy lifecycle test fixtures.
	if files, ok := c.ManagedAgents.(claudemanaged.FileClient); ok {
		return r.WithFiles(files, c.Prompts.FileSystem())
	}
	return r
}
