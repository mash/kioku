// Package runner defines the lifecycle of a wiki generation attempt.
package runner

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type AttemptKey struct{ JobID, ProjectID string }

// RunRef points at a run and the resources needed for cleanup.
type RunRef struct {
	// UsesProviderFiles distinguishes file-based managed runs from HTTP runs.
	UsesProviderFiles bool
	ID                string
	// UploadStarted records that an input upload may exist before its ID is known.
	UploadStarted bool
	InputFileID   string
}

type Attempt struct {
	Key AttemptKey
	Run RunRef
}

type State int

const (
	StateUnknown State = iota
	StateNone
	StateRunning
	StateStopped
)

// Secret never prints its value.
type Secret string

func (Secret) GoString() string             { return "[redacted]" }
func (Secret) String() string               { return "[redacted]" }
func (Secret) LogValue() slog.Value         { return slog.StringValue("[redacted]") }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type FileRunner interface {
	Runner
	FilePaths() (input, output string)
	CollectOutput(context.Context, Attempt, Status, int64) ([]byte, error)
}

var ErrOutputPending = errors.New("agent output is not available yet")

type RunSpec struct {
	Key                 AttemptKey
	InputURL, OutputURL string
	Token               Secret
	FirstMessage        string
	System              string
	BudgetCents         int64
	Deadline            time.Time
	InputArchive        []byte `json:"-"`
	// SaveRef records provider resources before another external side effect.
	SaveRef func(RunRef) error `json:"-"`
}

type Status struct {
	Usage         UsageSample
	Run           RunRef
	State         State
	Reason        string
	Failed        bool
	BudgetReached bool // The provider explicitly stopped at its budget.
	TurnID        string
}

var ErrTemporary = errors.New("managed agents: temporary failure")

type Runner interface {
	Start(context.Context, RunSpec) (RunRef, error)
	Inspect(context.Context, Attempt) (Status, error)
	Stop(context.Context, Attempt) error
	Cleanup(context.Context, Attempt) error
}

// ErrNotConfigured reports that generation has no configured runner.
var ErrNotConfigured = errors.New("runner is not configured")
