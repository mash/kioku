package model

import (
	"time"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/runner"
)

type GenerationMode string

const (
	ModeIncremental GenerationMode = "incremental"
	ModeFull        GenerationMode = "full"
)

type ManagedPhase string

const (
	PhasePreparing      ManagedPhase = "preparing"
	PhaseAgentRunning   ManagedPhase = "agent_running"
	PhaseOutputReceived ManagedPhase = "output_received"
	PhaseFinalizing     ManagedPhase = "finalizing"
)

type AgentRun struct {
	InboxIDs           []string       `json:"inbox_ids"`
	Ref                runner.RunRef  `json:"ref"`
	CostCents          *int64         `json:"cost_cents,omitempty"`
	CleanupComplete    bool           `json:"cleanup_complete,omitempty"`
	AbortReason        string         `json:"abort_reason,omitempty"`
	Phase              ManagedPhase   `json:"phase"`
	Mode               GenerationMode `json:"mode"`
	PreparedAt         time.Time      `json:"prepared_at"`
	Deadline           time.Time      `json:"deadline"`
	TokenID            string         `json:"token_id,omitempty"`
	Attempt            Attempt        `json:"attempt"`
	BudgetCents        int64          `json:"budget_cents"`
	BaseRevision       string         `json:"base_revision,omitempty"`
	SourceIDs          []string       `json:"source_ids"`
	SourceVersion      int64          `json:"source_version"`
	NextIntakeSequence int64          `json:"next_intake_sequence"`
	SourcesUpdatedAt   time.Time      `json:"sources_updated_at"`
	OutputKey          blob.Key       `json:"output_key,omitempty"`
	OutputHash         string         `json:"output_hash,omitempty"`
}

// Attempt identifies one run and its resources during recovery.
type Attempt struct {
	ID string `json:"id"`
}
