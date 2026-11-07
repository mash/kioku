// Package managed implements generation with Anthropic Managed Agents.
package managed

import (
	"context"
	"errors"
	"log/slog"
	"net/url"

	"github.com/mash/kioku/internal/managedagents"
	"github.com/mash/kioku/internal/runner"
)

const tokenName = "KIOKU_JOB_TOKEN"

type Runner struct {
	client        func() (managedagents.Client, error)
	agentID       string
	agentVersion  int64
	environmentID string
}

func New(client func() (managedagents.Client, error), agentID string, agentVersion int64, environmentID string) *Runner {
	return &Runner{client: client, agentID: agentID, agentVersion: agentVersion, environmentID: environmentID}
}

func (r *Runner) Start(ctx context.Context, spec runner.RunSpec) (runner.RunRef, error) {
	client, err := r.client()
	if err != nil {
		return runner.RunRef{}, err
	}
	u, err := url.Parse(spec.OutputURL)
	if err != nil {
		return runner.RunRef{}, err
	}
	vault, err := client.CreateJobVault(ctx, managedagents.VaultInput{JobID: spec.Key.JobID, AttemptID: spec.Key.AttemptID, ProjectID: spec.Key.ProjectID, SecretName: tokenName, SecretValue: string(spec.Token), AllowedHost: u.Hostname()})
	if err == nil {
		var id string
		id, err = client.CreateSession(ctx, managedagents.SessionInput{MAAgentID: r.agentID, MAAgentVersion: r.agentVersion, EnvironmentID: r.environmentID, VaultID: vault, BudgetCents: spec.BudgetCents, Title: "Kioku wiki " + spec.Key.JobID, Metadata: map[string]string{"kioku_project_id": spec.Key.ProjectID, "kioku_job_id": spec.Key.JobID, "kioku_attempt_id": spec.Key.AttemptID}, FirstMessage: spec.FirstMessage})
		if err == nil {
			return runner.RunRef{ID: id, Resources: map[string]string{"vault_id": vault}}, nil
		}
	}
	if errors.Is(err, runner.ErrTemporary) {
		slog.Warn("agent creation uncertain; reconciliation will locate it", "job", spec.Key.JobID)
	}
	return runner.RunRef{}, err
}

func (r *Runner) Inspect(ctx context.Context, a runner.Attempt) (runner.Status, error) {
	client, err := r.client()
	if err != nil {
		return runner.Status{}, err
	}
	if a.Run.ID == "" {
		a.Run.ID, err = client.FindSessionByJob(ctx, a.Key.JobID, a.Key.AttemptID)
		if err != nil {
			return runner.Status{}, err
		}
		if a.Run.ID == "" {
			return runner.Status{State: runner.StateNone}, nil
		}
	}
	session, err := client.GetSession(ctx, a.Run.ID)
	if err != nil {
		return runner.Status{}, err
	}
	out := runner.Status{Run: a.Run}
	if session.CostKnown {
		out.Cost = &session.ListCostCents
	}
	switch session.Status {
	case "running", "rescheduling":
		out.State = runner.StateRunning
	case "idle":
		out.State = runner.StateStopped
		switch session.LastStopReason {
		case "retries_exhausted":
			out.Reason = "agent gave up after errors"
		case "requires_action":
			out.Reason = "agent requires action"
		}
	case "terminated":
		out.State, out.Reason = runner.StateStopped, "session terminated"
	}
	return out, nil
}

func (r *Runner) Stop(ctx context.Context, a runner.Attempt) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	if a.Run.ID == "" {
		return nil
	}
	return client.Interrupt(ctx, a.Run.ID)
}

func (r *Runner) Cleanup(ctx context.Context, a runner.Attempt) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	var errs []error
	record := func(message string, err error) {
		if err != nil {
			slog.Warn(message, "job", a.Key.JobID, "error", err)
			errs = append(errs, err)
		}
	}
	if a.Run.ID == "" {
		a.Run.ID, err = client.FindSessionByJob(ctx, a.Key.JobID, a.Key.AttemptID)
		record("agent session discovery failed", err)
	}
	if a.Run.ID != "" {
		record("agent session archive failed", client.ArchiveSession(ctx, a.Run.ID))
	}
	vaults := []string{a.Run.Resources["vault_id"]}
	if vaults[0] == "" {
		vaults, err = client.FindJobVaults(ctx, a.Key.JobID, a.Key.AttemptID)
		record("agent vault discovery failed", err)
	}
	for _, id := range vaults {
		record("agent vault archive failed", client.ArchiveVault(ctx, id))
	}
	return errors.Join(errs...)
}
