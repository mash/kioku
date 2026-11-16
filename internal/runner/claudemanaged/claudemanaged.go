// Package claudemanaged implements generation with Anthropic Managed Agents.
package claudemanaged

import (
	"context"
	"errors"
	"log/slog"
	"net/url"

	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/prompts"
)

const tokenName = "KIOKU_JOB_TOKEN"

type Runner struct {
	system        string
	model         prompts.Model
	client        func() (Client, error)
	agentID       string
	agentVersion  int64
	environmentID string
}

func New(client func() (Client, error), agentID string, agentVersion int64, environmentID string, system string, model prompts.Model) *Runner {
	return &Runner{system: system, model: model, client: client, agentID: agentID, agentVersion: agentVersion, environmentID: environmentID}
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
	credentials, err := runner.VaultCredentials(spec, u.Hostname())
	if err != nil {
		return runner.RunRef{}, err
	}
	vault, err := client.CreateJobVault(ctx, VaultInput{JobID: spec.Key.JobID, AttemptID: spec.Key.AttemptID, ProjectID: spec.Key.ProjectID, Credentials: credentials})
	if err == nil {
		var id string
		system := spec.System
		if system == "" {
			system = r.system
		}
		id, err = client.CreateSession(ctx, SessionInput{System: system, Model: r.model, MAAgentID: r.agentID, MAAgentVersion: r.agentVersion, EnvironmentID: r.environmentID, VaultID: vault, BudgetCents: spec.BudgetCents, Title: "Kioku wiki " + spec.Key.JobID, Metadata: map[string]string{"kioku_job_id": spec.Key.JobID, "kioku_attempt_id": spec.Key.AttemptID}, FirstMessage: spec.FirstMessage})
		var rejected *APIError
		if errors.As(err, &rejected) && rejected.Status >= 400 && rejected.Status < 500 {
			slog.Error("session create rejected", "status", rejected.Status, "type", rejected.Type)
		}
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
	ids, err := discoverSessions(ctx, client, a)
	if err != nil {
		return runner.Status{}, err
	}
	return inspectSessions(ctx, client, a, ids, false)
}

func (r *Runner) Stop(ctx context.Context, a runner.Attempt) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	ids, err := discoverSessions(ctx, client, a)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := client.Interrupt(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
	ids, err := discoverSessions(ctx, client, a)
	if err != nil {
		return err
	}
	if err = confirmCleanupStopped(ctx, client, a, ids, false); err != nil {
		return err
	}
	for _, id := range ids {
		record("agent session archive failed", client.ArchiveSession(ctx, id))
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
