package model

import (
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/runner"
)

type ManagedPhase string

const (
	PhasePreparing      ManagedPhase = "preparing"
	PhaseAgentRunning   ManagedPhase = "agent_running"
	PhaseOutputReceived ManagedPhase = "output_received"
	PhaseFinalizing     ManagedPhase = "finalizing"
)

type AgentRun struct {
	GitHubCredentials bool          `json:"github_credentials,omitempty"`
	InboxIDs          []string      `json:"inbox_ids"`
	Ref               runner.RunRef `json:"ref"`
	CostCents         *int64        `json:"cost_cents,omitempty"`
	CleanupComplete   bool          `json:"cleanup_complete,omitempty"`
	AbortReason       string        `json:"abort_reason,omitempty"`
	Phase             ManagedPhase  `json:"phase"`
	PreparedAt        time.Time     `json:"prepared_at"`
	Deadline          time.Time     `json:"deadline"`
	TokenID           string        `json:"token_id,omitempty"`
	Attempt           Attempt       `json:"attempt"`
	BudgetCents       int64         `json:"budget_cents"`
	BaseRevision      string        `json:"base_revision,omitempty"`
	OutputKey         blob.Key      `json:"output_key,omitempty"`
	OutputHash        string        `json:"output_hash,omitempty"`
}

// Attempt identifies one run and its resources during recovery.
type Attempt struct {
	ID string `json:"id"`
}
