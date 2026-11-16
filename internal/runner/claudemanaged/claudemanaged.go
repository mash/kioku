// Package claudemanaged implements generation with Anthropic Managed Agents.
package claudemanaged

import (
	"github.com/mash/kioku/prompts"
)

type Runner struct {
	system        string
	model         prompts.Model
	files         FileClient
	agentID       string
	agentVersion  int64
	environmentID string
}

func New(client FileClient, agentID string, agentVersion int64, environmentID string, system string, model prompts.Model) *Runner {
	return &Runner{system: system, model: model, files: client, agentID: agentID, agentVersion: agentVersion, environmentID: environmentID}
}
