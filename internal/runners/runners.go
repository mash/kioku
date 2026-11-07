// Package runners configures and selects generation runners.
package runners

import (
	"context"
	"errors"
	"slices"
	"time"

	execrunner "github.com/mash/kioku/internal/runner/exec"

	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/internal/runner/claudemanaged"
	openairunner "github.com/mash/kioku/internal/runner/openai"
)

type Config struct {
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
	ManagedAgents        managedagents.Client
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
	if job.Run != nil && job.Run.Ref.Resources["provider"] == "openai" {
		if s.openaiRunner == nil {
			return nil, errors.New("OpenAI runner unavailable")
		}
		return s.openaiRunner, nil
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
	return claudemanaged.New(func() (managedagents.Client, error) { return s.config.ManagedAgents, nil }, s.config.MAAgentID, s.config.MAAgentVersion, s.config.MAEnvironmentID), nil
}
