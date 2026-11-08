package claudemanaged

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mash/kioku/internal/runner"
)

type FileRunner struct {
	*Runner
	files  FileClient
	system string
}

func (r *Runner) WithFiles(client FileClient, system string) *FileRunner {
	return &FileRunner{Runner: r, files: client, system: system}
}

func (*FileRunner) Provider() string { return "claudemanaged" }

func (*FileRunner) FilePaths() (string, string) {
	return "/mnt/session/uploads/input.tar.gz", "/mnt/session/outputs/" + runner.OutputArchiveName
}

func (r *FileRunner) Start(ctx context.Context, spec runner.RunSpec) (runner.RunRef, error) {
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
	return runner.StartFileSession(ctx, spec, r.Provider(), r.files.UploadInput,
		func(ctx context.Context, id string, _ func(string) error) (string, error) {
			if !spec.Deadline.IsZero() && time.Now().After(spec.Deadline) {
				return "", errors.New("Claude job deadline exceeded")
			}
			if err := r.files.CheckOfflineEnvironment(ctx, r.environmentID); err != nil {
				return "", err
			}
			return r.files.CreateSession(ctx, SessionInput{
				System: r.system, Model: r.model, MAAgentID: r.agentID, MAAgentVersion: version,
				EnvironmentID: r.environmentID, InputFileID: id, BudgetCents: spec.BudgetCents,
				Title: "Kioku wiki " + spec.Key.JobID, FirstMessage: spec.FirstMessage,
				Metadata: map[string]string{"kioku_job_id": spec.Key.JobID, "kioku_attempt_id": spec.Key.AttemptID},
			})
		})
}

func (r *FileRunner) sessions(ctx context.Context, a runner.Attempt) ([]string, error) {
	if a.Run.ID != "" {
		return []string{a.Run.ID}, nil
	}
	if a.Run.Resources["upload_started"] != "true" {
		return nil, nil
	}
	return r.files.FindAttemptSessions(ctx, a.Key)
}

func (r *FileRunner) Inspect(ctx context.Context, a runner.Attempt) (runner.Status, error) {
	if !a.Run.UsesProviderFiles {
		return r.Runner.Inspect(ctx, a)
	}
	ids, err := r.sessions(ctx, a)
	if err != nil {
		return runner.Status{}, err
	}
	if len(ids) == 0 {
		return runner.Status{State: runner.StateNone}, nil
	}
	if len(ids) != 1 {
		return runner.Status{}, errors.New("multiple Claude sessions for one attempt")
	}
	a.Run.ID = ids[0]
	session, err := r.files.GetSession(ctx, a.Run.ID)
	if isNotFound(err) {
		return runner.Status{Run: a.Run, State: runner.StateNone}, nil
	}
	if err != nil {
		return runner.Status{}, err
	}
	meta := session.Metadata
	if session.ID != a.Run.ID || meta["kioku_job_id"] != a.Key.JobID || meta["kioku_attempt_id"] != a.Key.AttemptID {
		return runner.Status{}, errors.New("Claude session does not match the generation attempt")
	}
	out := runner.Status{Run: a.Run}
	if session.CostKnown {
		out.Cost = &session.ListCostCents
	}
	switch session.Status {
	case "running", "rescheduling":
		out.State = runner.StateRunning
	case "idle":
		switch session.LastStopReason {
		case "end_turn":
			out.State = runner.StateStopped
		case "":
			// The end-turn event can appear shortly after the session goes idle.
			out.State = runner.StateRunning
		default:
			out.State, out.Reason = runner.StateStopped, "Claude session did not finish successfully"
		}
	case "terminated":
		out.State, out.Reason = runner.StateStopped, "Claude session terminated"
	}
	return out, nil
}

func (r *FileRunner) CollectOutput(ctx context.Context, a runner.Attempt, status runner.Status, limit int64) ([]byte, error) {
	if a.Run.ID == "" || status.Run.ID != a.Run.ID || status.State != runner.StateStopped || status.Reason != "" {
		return nil, errors.New("Claude output requires a matching completed session")
	}
	return r.files.DownloadOutput(ctx, a.Run.ID, limit)
}

func (r *FileRunner) Stop(ctx context.Context, a runner.Attempt) error {
	if !a.Run.UsesProviderFiles {
		return r.Runner.Stop(ctx, a)
	}
	ids, err := r.sessions(ctx, a)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		err = r.files.Interrupt(ctx, id)
		if !isNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *FileRunner) Cleanup(ctx context.Context, a runner.Attempt) error {
	if !a.Run.UsesProviderFiles {
		return r.Runner.Cleanup(ctx, a)
	}
	ids, err := r.sessions(ctx, a)
	if err != nil {
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
