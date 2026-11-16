// Package runners configures and selects generation runners.
package runners

import (
	"context"

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

// Set holds the runner selected at startup.
type Set struct {
	// Runner is nil when no runner is configured.
	Runner runner.Runner
	exec   *execrunner.Runner
}

// Close stops exec children. The other runners hold no local processes.
func (s *Set) Close(ctx context.Context) error {
	if s.exec == nil {
		return nil
	}
	return s.exec.Close(ctx)
}
