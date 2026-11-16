// Package runners configures and selects generation runners.
package runners

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/mash/kioku/prompts"

	execrunner "github.com/mash/kioku/internal/runner/exec"
	"github.com/mash/kioku/internal/store"

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
	ManagedAgents        claudemanaged.FileClient
	BaseURL              string
	AgentMaxSessions     int
	AgentJobTimeout      time.Duration
	AgentMaxSessionCents int64
}

type Set struct {
	config       Config
	execRunner   *execrunner.Runner
	openaiRunner *openairunner.Runner
}

func Load(c Config, onExit func(string)) (*Set, error) {
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

// Select the recorded provider before the configured default.
func (s *Set) New(job store.JobRow) (runner.Runner, error) {
	if job.Data.Run != nil {
		provider := ""
		if job.Data.Run.Provider != nil {
			provider = *job.Data.Run.Provider
		}
		switch provider {
		case "openai":
			if s.openaiRunner == nil {
				return nil, errors.New("OpenAI runner unavailable")
			}
			return s.openaiRunner, nil
		case "claudemanaged":
			if s.config.ManagedAgents == nil {
				return nil, errors.New("Claude file runner unavailable")
			}
			return s.claudeRunner(), nil
		}
	}
	if s.config.Type == "openai" {
		if job.Data.Run != nil && job.SessionID != nil {
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
	return claudemanaged.New(c.ManagedAgents, c.MAAgentID, c.MAAgentVersion, c.MAEnvironmentID, c.Prompts.FileSystem(), c.Prompts.Model())
}
