package model

import (
	"time"
)

type ManagedPhase string

const (
	PhasePreparing      ManagedPhase = "preparing"
	PhaseAgentRunning   ManagedPhase = "agent_running"
	PhaseOutputReceived ManagedPhase = "output_received"
	PhaseFinalizing     ManagedPhase = "finalizing"
)

// AgentRun is the public run projection. Provider resources and private
// checkpoints live only in the typed SQL job row.
type AgentRun struct {
	CostCents       *int64       `json:"cost_cents"`
	CleanupComplete bool         `json:"cleanup_complete"`
	Phase           ManagedPhase `json:"phase"`
	PreparedAt      time.Time    `json:"prepared_at"`
	Deadline        time.Time    `json:"deadline"`
	BudgetCents     int64        `json:"budget_cents"`
}
