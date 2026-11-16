package claudemanaged

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mash/kioku/internal/runner"
)

func (*Runner) Provider() string { return "claudemanaged" }

func (*Runner) FilePaths() (string, string) {
	return "/mnt/session/uploads/input.tar.gz", "/mnt/session/outputs/" + runner.OutputArchiveName
}

func (r *Runner) Start(ctx context.Context, spec runner.RunSpec) (runner.RunRef, error) {
	if spec.BudgetCents < 50 || len(spec.InputArchive) > 500_000_000 {
		return runner.RunRef{}, errors.New("Claude generation budget or input archive size is out of range")
	}
	version, err := r.files.OfflineAgentVersion(ctx, r.agentID, r.agentVersion)
	if err != nil {
		return runner.RunRef{}, err
	}
	if err = r.files.CheckOfflineEnvironment(ctx, r.environmentID); err != nil {
		return runner.RunRef{}, err
	}
	system := spec.System
	if system == "" {
		system = r.system
	}
	return runner.StartFileSession(ctx, spec, r.Provider(), r.files.UploadInput,
		func(ctx context.Context, id string, _ func(string) error) (string, error) {
			if !spec.Deadline.IsZero() && time.Now().After(spec.Deadline) {
				return "", errors.New("Claude job deadline exceeded")
			}
			if err := r.files.CheckOfflineEnvironment(ctx, r.environmentID); err != nil {
				return "", err
			}
			return r.files.CreateSession(ctx, SessionInput{
				System: system, Model: r.model, MAAgentID: r.agentID, MAAgentVersion: version,
				EnvironmentID: r.environmentID, InputFileID: id, BudgetCents: spec.BudgetCents,
				Title: "Kioku wiki " + spec.Key.JobID, FirstMessage: spec.FirstMessage,
				Metadata: map[string]string{"kioku_job_id": spec.Key.JobID, "kioku_attempt_id": spec.Key.AttemptID},
			})
		})
}

func (r *Runner) sessions(ctx context.Context, a runner.Attempt) ([]string, error) {
	if a.Run.ID == "" && a.Run.Resources["upload_started"] != "true" {
		return nil, nil
	}
	return discoverSessions(ctx, r.files, a)
}
func (r *Runner) Inspect(ctx context.Context, a runner.Attempt) (runner.Status, error) {
	ids, err := r.sessions(ctx, a)
	if err != nil {
		return runner.Status{}, err
	}
	return inspectSessions(ctx, r.files, a, ids)
}

func (r *Runner) CollectOutput(ctx context.Context, a runner.Attempt, status runner.Status, limit int64) ([]byte, error) {
	if a.Run.ID == "" || status.Run.ID != a.Run.ID || status.State != runner.StateStopped || status.Reason != "" {
		return nil, errors.New("Claude output requires a matching completed session")
	}
	return r.files.DownloadOutput(ctx, a.Run.ID, limit)
}

func (r *Runner) Stop(ctx context.Context, a runner.Attempt) error {
	ids, err := r.sessions(ctx, a)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		errs = append(errs, r.files.Interrupt(ctx, id))
	}
	return errors.Join(errs...)
}

func (r *Runner) Cleanup(ctx context.Context, a runner.Attempt) error {
	ids, err := r.sessions(ctx, a)
	if err != nil {
		return err
	}
	if err = confirmCleanupStopped(ctx, r.files, a, ids); err != nil {
		return err
	}
	for _, id := range ids {
		if err := r.files.DeleteSession(ctx, id); err != nil {
			return fmt.Errorf("delete Claude session: %w", err)
		}
	}
	var files []string
	if id := a.Run.Resources["input_file_id"]; id != "" {
		files = []string{id}
	} else if a.Run.Resources["upload_started"] == "true" {
		files, err = r.files.FindInputs(ctx, runner.InputArchiveName(a.Key))
		if err != nil {
			return err
		}
	}
	for _, id := range files {
		if err := r.files.DeleteFile(ctx, id); err != nil {
			return fmt.Errorf("delete Claude input: %w", err)
		}
	}
	return nil
}
