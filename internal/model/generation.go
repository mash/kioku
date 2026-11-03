package model

import "time"

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

type ManagedRun struct {
	StopPending        bool           `json:"stop_pending,omitempty"`
	CleanupComplete    bool           `json:"cleanup_complete,omitempty"`
	AbortReason        string         `json:"abort_reason,omitempty"`
	Phase              ManagedPhase   `json:"phase"`
	Mode               GenerationMode `json:"mode"`
	PreparedAt         time.Time      `json:"prepared_at"`
	Deadline           time.Time      `json:"deadline"`
	SessionID          string         `json:"session_id,omitempty"`
	VaultID            string         `json:"vault_id,omitempty"`
	TokenID            string         `json:"token_id,omitempty"`
	Attempt            Attempt        `json:"hold"`
	BudgetCents        int64          `json:"budget_cents"`
	MAEnvironmentID    string         `json:"ma_environment_id,omitempty"`
	MAAgentID          string         `json:"ma_agent_id"`
	MAAgentVersion     int64          `json:"ma_agent_version,omitempty"`
	BaseRevision       string         `json:"base_revision,omitempty"`
	SourceIDs          []string       `json:"source_ids"`
	SourceVersion      int64          `json:"source_version"`
	NextIntakeSequence int64          `json:"next_intake_sequence"`
	SourcesUpdatedAt   time.Time      `json:"sources_updated_at"`
	OutputKey          string         `json:"output_key,omitempty"`
	OutputHash         string         `json:"output_hash,omitempty"`
	ListCostCents      int64          `json:"list_cost_cents,omitempty"`
}

// Attempt identifies one managed agent run. Its ID tags the vault and the
// session so that recovery finds the resources of this run only. The JSON
// name "hold" matches records that earlier versions stored.
type Attempt struct {
	ID string `json:"id"`
}
