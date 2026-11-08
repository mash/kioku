package claudemanaged

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/mash/kioku/internal/runner"
)

type FileClient interface {
	Client
	CheckOfflineEnvironment(context.Context, string) error
	OfflineAgentVersion(context.Context, string, int64) (int64, error)
	UploadInput(context.Context, string, []byte) (string, error)
	FindInputs(context.Context, string) ([]string, error)
	FindAttemptSessions(context.Context, runner.AttemptKey) ([]string, error)
	DownloadOutput(context.Context, string, int64) ([]byte, error)
	DeleteFile(context.Context, string) error
	DeleteSession(context.Context, string) error
}

func (a *API) CheckOfflineEnvironment(ctx context.Context, id string) error {
	env, err := a.sdk.Environments.Get(ctx, id, anthropic.BetaEnvironmentGetParams{})
	if err != nil {
		return apiError(err)
	}
	config := env.Config
	network := config.Networking
	if env.ID != id || env.ArchivedAt != "" || config.Type != "cloud" || network.Type != "limited" ||
		len(network.AllowedHosts) != 0 || network.AllowMCPServers || network.AllowPackageManagers {
		return errors.New("Claude generation requires a cloud environment with no outbound hosts, MCP access, or package manager access")
	}
	return nil
}

func (a *API) OfflineAgentVersion(ctx context.Context, id string, version int64) (int64, error) {
	params := anthropic.BetaAgentGetParams{}
	if version > 0 {
		params.Version = anthropic.Int(version)
	}
	agent, err := a.sdk.Agents.Get(ctx, id, params)
	if err != nil {
		return 0, apiError(err)
	}
	if agent.ID != id || agent.Version < 1 || version > 0 && agent.Version != version ||
		!agent.ArchivedAt.IsZero() || agent.Multiagent.Type != "" ||
		agent.ExecutionIdentity.Type != "" && agent.ExecutionIdentity.Type != "service_account" {
		return 0, errors.New("Claude generation requires a single agent with the default execution identity")
	}
	return agent.Version, nil
}

var managedFilesBetas = []anthropic.AnthropicBeta{"files-api-2025-04-14", "managed-agents-2026-04-01"}

func (a *API) UploadInput(ctx context.Context, name string, body []byte) (string, error) {
	file, err := a.sdk.Files.Upload(ctx, anthropic.BetaFileUploadParams{
		File:             anthropic.File(bytes.NewReader(body), name, "application/gzip"),
		ExpiresInSeconds: anthropic.Int(int64((24 * time.Hour).Seconds())),
		Betas:            managedFilesBetas,
	})
	if err != nil {
		return "", apiError(err)
	}
	return file.ID, nil
}

func (a *API) eachFile(ctx context.Context, scope string, visit func(anthropic.BetaFileMetadata) error) error {
	params := anthropic.BetaFileListParams{Betas: managedFilesBetas}
	if scope != "" {
		params.ScopeID = anthropic.String(scope)
	}
	seen := map[string]bool{}
	for {
		page, err := a.sdk.Files.List(ctx, params)
		if err != nil {
			return apiError(err)
		}
		for _, file := range page.Data {
			if err := visit(file); err != nil {
				return err
			}
		}
		if page.NextPage == "" {
			return nil
		}
		if seen[page.NextPage] {
			return fmt.Errorf("%w: repeated Claude files cursor", ErrTemporary)
		}
		seen[page.NextPage] = true
		params.Page = anthropic.String(page.NextPage)
	}
}

func (a *API) FindInputs(ctx context.Context, name string) ([]string, error) {
	var ids []string
	err := a.eachFile(ctx, "", func(file anthropic.BetaFileMetadata) error {
		if file.Filename == name && file.Scope.ID == "" && file.ID != "" {
			ids = append(ids, file.ID)
		}
		return nil
	})
	return ids, err
}

func (a *API) FindAttemptSessions(ctx context.Context, key runner.AttemptKey) ([]string, error) {
	if key.JobID == "" || key.AttemptID == "" || key.ProjectID == "" {
		return nil, errors.New("missing Claude attempt identity")
	}
	var ids []string
	params := anthropic.BetaSessionListParams{}
	seen := map[string]bool{}
	for {
		page, err := a.sdk.Sessions.List(ctx, params)
		if err != nil {
			return nil, apiError(err)
		}
		for _, session := range page.Data {
			meta := session.Metadata
			if meta["kioku_job_id"] == key.JobID && meta["kioku_attempt_id"] == key.AttemptID && session.ID != "" {
				ids = append(ids, session.ID)
			}
		}
		if page.NextPage == "" {
			return ids, nil
		}
		if seen[page.NextPage] {
			return nil, fmt.Errorf("%w: repeated Claude session cursor", ErrTemporary)
		}
		seen[page.NextPage] = true
		params.Page = anthropic.String(page.NextPage)
	}
}

func (a *API) DownloadOutput(ctx context.Context, session string, limit int64) ([]byte, error) {
	var id string
	err := a.eachFile(ctx, session, func(file anthropic.BetaFileMetadata) error {
		if file.Filename != runner.OutputArchiveName || !file.Downloadable {
			return nil
		}
		if file.Scope.ID != session || file.Scope.Type != "session" || file.ID == "" {
			return errors.New("Claude output scope does not match the session")
		}
		if id != "" {
			return errors.New("multiple Claude output archives for one session")
		}
		if file.SizeBytes > limit {
			return errors.New("Claude output archive exceeds the compressed size limit")
		}
		id = file.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, runner.ErrOutputPending
	}
	res, err := a.sdk.Files.Download(ctx, id, anthropic.BetaFileDownloadParams{Betas: managedFilesBetas})
	if err != nil {
		return nil, apiError(err)
	}
	defer res.Body.Close()
	return runner.ReadOutputArchive(res.Body, limit)
}

func isNotFound(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == 404
}

func (a *API) DeleteFile(ctx context.Context, id string) error {
	_, err := a.sdk.Files.Delete(ctx, id, anthropic.BetaFileDeleteParams{Betas: managedFilesBetas})
	err = apiError(err)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (a *API) DeleteSession(ctx context.Context, id string) error {
	// The provider permanently deletes all session-scoped files with the session.
	_, err := a.sdk.Sessions.Delete(ctx, id, anthropic.BetaSessionDeleteParams{})
	err = apiError(err)
	if isNotFound(err) {
		return nil
	}
	return err
}
