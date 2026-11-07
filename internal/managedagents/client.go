package managedagents

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/mash/kioku/internal/runner"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

type Client interface {
	CreateJobVault(context.Context, VaultInput) (string, error)
	FindJobVaults(context.Context, string, string) ([]string, error)
	ArchiveVault(context.Context, string) error
	CreateSession(context.Context, SessionInput) (string, error)
	GetSession(context.Context, string) (Session, error)
	FindSessionByJob(context.Context, string, ...string) (string, error)
	Interrupt(context.Context, string) error
	ArchiveSession(context.Context, string) error
}
type VaultInput struct{ JobID, AttemptID, ProjectID, SecretName, SecretValue, AllowedHost string }
type SessionInput struct {
	MAAgentID              string
	MAAgentVersion         int64
	EnvironmentID, VaultID string
	BudgetCents            int64
	Title                  string
	Metadata               map[string]string
	FirstMessage           string
}
type Session struct {
	ID, Status, LastStopReason string
	ListCostCents              int64
	CostKnown                  bool
	Metadata                   map[string]string
}

var ErrTemporary = runner.ErrTemporary

type API struct{ sdk anthropic.BetaService }

func New(baseURL, apiKey string, client *http.Client, workspace ...string) *API {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// Service construction deliberately avoids SDK environment credential/header
	// discovery. Only the configured server key and explicit origin are used.
	opts := []option.RequestOption{option.WithBaseURL(baseURL), option.WithAPIKey(apiKey), option.WithHTTPClient(&copyClient), option.WithMaxRetries(0), option.WithHeader("anthropic-version", "2023-06-01")}
	if len(workspace) > 0 && workspace[0] != "" {
		opts = append(opts, option.WithHeader("anthropic-workspace-id", workspace[0]))
	}
	sdk := anthropic.NewBetaService(opts...)
	return &API{sdk: sdk}
}
func apiError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	var e *anthropic.Error
	if errors.As(err, &e) {
		if e.StatusCode == 429 || e.StatusCode >= 500 {
			return fmt.Errorf("%w: HTTP %d", ErrTemporary, e.StatusCode)
		}
		return fmt.Errorf("managed agents: HTTP %d", e.StatusCode)
	}
	// SDK errors can contain provider response bodies or credential-bearing
	// request dumps. Never propagate those into logs or persisted job errors.
	return fmt.Errorf("%w: request failed", ErrTemporary)
}
func (a *API) CreateJobVault(ctx context.Context, in VaultInput) (string, error) {
	v, err := a.sdk.Vaults.New(ctx, anthropic.BetaVaultNewParams{DisplayName: "kioku-job-" + in.JobID, Metadata: map[string]string{"kioku_job_id": in.JobID, "kioku_attempt_id": in.AttemptID, "kioku_project_id": in.ProjectID}})
	if err != nil {
		return "", apiError(err)
	}
	if v.ID == "" {
		return "", errors.New("managed agents: missing vault ID")
	}
	_, err = a.sdk.Vaults.Credentials.New(ctx, v.ID, anthropic.BetaVaultCredentialNewParams{Auth: anthropic.BetaVaultCredentialNewParamsAuthUnion{OfEnvironmentVariable: &anthropic.BetaManagedAgentsEnvironmentVariableCreateParams{SecretName: in.SecretName, SecretValue: in.SecretValue, Networking: anthropic.BetaManagedAgentsCredentialNetworkingParamsOfLimited([]string{in.AllowedHost}), InjectionLocation: anthropic.BetaManagedAgentsInjectionLocationParams{Header: anthropic.Bool(true)}}}})
	// Return the vault even on credential failure so the caller can persist and archive it.
	return v.ID, apiError(err)
}

// FindJobVaults recovers vaults whose create response or local ID commit was lost.
// The hold ID distinguishes retries that share a job ID.
func (a *API) FindJobVaults(ctx context.Context, jobID, attemptID string) ([]string, error) {
	if jobID == "" || attemptID == "" {
		return nil, errors.New("managed agents: vault lookup requires job and attempt IDs")
	}
	var ids []string
	cursor := ""
	seen := map[string]bool{}
	for {
		params := anthropic.BetaVaultListParams{}
		if cursor != "" {
			params.Page = anthropic.String(cursor)
		}
		page, err := a.sdk.Vaults.List(ctx, params)
		if err != nil {
			return nil, apiError(err)
		}
		for _, vault := range page.Data {
			if vault.Metadata["kioku_job_id"] == jobID && vault.Metadata["kioku_attempt_id"] == attemptID && vault.ArchivedAt.IsZero() {
				ids = append(ids, vault.ID)
			}
		}
		if page.NextPage == "" {
			return ids, nil
		}
		if seen[page.NextPage] {
			return nil, fmt.Errorf("%w: repeated vault cursor", ErrTemporary)
		}
		seen[page.NextPage] = true
		cursor = page.NextPage
	}
}
func (a *API) ArchiveVault(ctx context.Context, id string) error {
	_, err := a.sdk.Vaults.Archive(ctx, id, anthropic.BetaVaultArchiveParams{})
	return apiError(err)
}
func (a *API) CreateSession(ctx context.Context, in SessionInput) (string, error) {
	if in.BudgetCents < 50 {
		return "", errors.New("managed agents: budget must be at least 50 cents")
	}
	agent := anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(in.MAAgentID)}
	if in.MAAgentVersion > 0 {
		agent = anthropic.BetaSessionNewParamsAgentUnion{OfBetaManagedAgentsAgents: &anthropic.BetaManagedAgentsAgentParams{ID: in.MAAgentID, Version: anthropic.Int(in.MAAgentVersion)}}
	}
	v, err := a.sdk.Sessions.New(ctx, anthropic.BetaSessionNewParams{Agent: agent, EnvironmentID: in.EnvironmentID, VaultIDs: []string{in.VaultID}, Metadata: in.Metadata, Title: anthropic.String(in.Title), Budget: anthropic.BetaManagedAgentsBudgetLimitParam{MaxListCost: anthropic.BetaMonetaryAmountParam{Amount: strconv.FormatInt(in.BudgetCents, 10), Currency: "USD"}}, InitialEvents: []anthropic.BetaSessionNewParamsInitialEventUnion{{OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{OfText: &anthropic.BetaManagedAgentsTextBlockParam{Text: in.FirstMessage}}}}}}})
	if err != nil {
		return "", apiError(err)
	}
	if v.ID == "" {
		return "", errors.New("managed agents: missing session ID")
	}
	return v.ID, nil
}
func (a *API) GetSession(ctx context.Context, id string) (Session, error) {
	v, err := a.sdk.Sessions.Get(ctx, id, anthropic.BetaSessionGetParams{})
	if err != nil {
		return Session{}, apiError(err)
	}
	s := Session{ID: v.ID, Status: string(v.Status), Metadata: v.Metadata}
	amount := v.Usage.ListCost.Amount
	cost, err := strconv.ParseInt(amount, 10, 64)
	if err == nil && cost >= 0 && v.Usage.ListCost.Currency == "USD" && cost <= math.MaxInt64/20000 {
		s.ListCostCents = cost
		s.CostKnown = true
	}
	var latest time.Time
	latestID := ""
	cursor := ""
	seen := map[string]bool{}
	for {
		params := anthropic.BetaSessionEventListParams{}
		if cursor != "" {
			params.Page = anthropic.String(cursor)
		}
		page, err := a.sdk.Sessions.Events.List(ctx, id, params)
		if err != nil {
			return s, fmt.Errorf("%w: session events unavailable", ErrTemporary)
		}
		for _, event := range page.Data {
			if event.Type == "session.status_idle" && (latestID == "" || event.ProcessedAt.After(latest) || event.ProcessedAt.Equal(latest) && event.ID > latestID) {
				latest = event.ProcessedAt
				latestID = event.ID
				s.LastStopReason = event.StopReason.Type
			}
		}
		if page.NextPage == "" {
			break
		}
		if seen[page.NextPage] {
			return s, fmt.Errorf("%w: repeated event cursor", ErrTemporary)
		}
		seen[page.NextPage] = true
		cursor = page.NextPage
	}
	return s, nil
}
func (a *API) FindSessionByJob(ctx context.Context, jobID string, attemptID ...string) (string, error) {
	if len(attemptID) > 1 {
		return "", errors.New("at most one attempt ID is allowed")
	}
	cursor := ""
	seen := map[string]bool{}
	id := ""
	var latest time.Time
	for {
		params := anthropic.BetaSessionListParams{}
		if cursor != "" {
			params.Page = anthropic.String(cursor)
		}
		page, err := a.sdk.Sessions.List(ctx, params)
		if err != nil {
			return "", apiError(err)
		}
		for _, s := range page.Data {
			if s.Metadata["kioku_job_id"] == jobID && (len(attemptID) == 0 || s.Metadata["kioku_attempt_id"] == attemptID[0]) && s.ArchivedAt.IsZero() && (id == "" || s.CreatedAt.After(latest) || s.CreatedAt.Equal(latest) && s.ID > id) {
				id = s.ID
				latest = s.CreatedAt
			}
		}
		if page.NextPage == "" {
			break
		}
		if seen[page.NextPage] {
			return "", fmt.Errorf("%w: repeated session cursor", ErrTemporary)
		}
		seen[page.NextPage] = true
		cursor = page.NextPage
	}
	return id, nil
}
func (a *API) Interrupt(ctx context.Context, id string) error {
	_, err := a.sdk.Sessions.Events.Send(ctx, id, anthropic.BetaSessionEventSendParams{Events: []anthropic.BetaManagedAgentsEventParamsUnion{{OfUserInterrupt: &anthropic.BetaManagedAgentsUserInterruptEventParams{}}}})
	return apiError(err)
}
func (a *API) ArchiveSession(ctx context.Context, id string) error {
	_, err := a.sdk.Sessions.Archive(ctx, id, anthropic.BetaSessionArchiveParams{})
	return apiError(err)
}

// VerifyWebhook uses the SDK's Standard Webhooks verifier, including its five
// minute timestamp tolerance. It never makes a network request.
func VerifyWebhook(key string, body []byte, headers http.Header) (eventType, sessionID string, err error) {
	service := anthropic.NewBetaWebhookService(option.WithWebhookKey(key))
	event, err := service.Unwrap(body, headers)
	if err != nil {
		return "", "", errors.New("invalid managed agents webhook")
	}
	return event.Data.Type, event.Data.ID, nil
}
