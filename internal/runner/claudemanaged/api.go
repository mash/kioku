package claudemanaged

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mash/kioku/internal/runner"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/mash/kioku/prompts"
)

type Client interface {
	CreateSession(context.Context, SessionInput) (string, error)
	GetSession(context.Context, string) (Session, error)
	FindAttemptSessions(context.Context, runner.AttemptKey) ([]string, error)
	Interrupt(context.Context, string) error
}
type SessionInput struct {
	System         string
	Model          prompts.Model
	MAAgentID      string
	MAAgentVersion int64
	EnvironmentID  string
	BudgetCents    int64
	Title          string
	Metadata       map[string]string
	FirstMessage   string
	InputFileID    string
}
type Session struct {
	ID, Status, LastStopReason string
	Usage                      runner.UsageSample
	Metadata                   map[string]string
}

var ErrTemporary = runner.ErrTemporary

type API struct{ sdk anthropic.BetaService }

func NewAPI(baseURL, apiKey string, client *http.Client) *API {
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
	sdk := anthropic.NewBetaService(opts...)
	return &API{sdk: sdk}
}

// APIError retains only HTTP status and error.type, never a provider body or request.
type APIError struct {
	Status int
	Type   string
}

func (e *APIError) Error() string { return fmt.Sprintf("managed agents: HTTP %d", e.Status) }
func (e *APIError) Is(target error) bool {
	return target == ErrTemporary && (e.Status == 429 || e.Status >= 500)
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
		return &APIError{Status: e.StatusCode, Type: string(e.Type())}
	}
	// SDK errors can contain provider response bodies or credential-bearing
	// request dumps. Never propagate those into logs or persisted job errors.
	return fmt.Errorf("%w: request failed", ErrTemporary)
}

func (a *API) CreateSession(ctx context.Context, in SessionInput) (string, error) {
	if in.BudgetCents < 50 {
		return "", errors.New("managed agents: budget must be at least 50 cents")
	}
	overrides := anthropic.BetaManagedAgentsAgentWithOverridesParams{
		ID: in.MAAgentID, System: anthropic.String(in.System),
		MCPServers: []anthropic.BetaManagedAgentsURLMCPServerParams{},
		Skills:     []anthropic.BetaManagedAgentsSkillParamsUnion{},
		Tools:      wikiTools,
	}
	if in.MAAgentVersion > 0 {
		overrides.Version = anthropic.Int(in.MAAgentVersion)
	}
	if in.Model.ID != "" {
		overrides.Model = anthropic.BetaManagedAgentsModelConfigParams{
			ID:    anthropic.BetaManagedAgentsModel(in.Model.ID),
			Speed: anthropic.BetaManagedAgentsModelConfigParamsSpeed(in.Model.Speed),
		}
		if in.Model.Effort != "" {
			overrides.Model.Effort.OfBetaManagedAgentsModelConfigsEffortBetaManagedAgentsEffortLevel = anthropic.String(in.Model.Effort)
		}
		if in.Model.InferenceGeo != "" {
			overrides.Model.InferenceGeo = anthropic.String(in.Model.InferenceGeo)
		}
	}
	agent := anthropic.BetaSessionNewParamsAgentUnion{OfBetaManagedAgentsAgentWithOverridess: &overrides}

	params := anthropic.BetaSessionNewParams{Agent: agent, EnvironmentID: in.EnvironmentID, Metadata: in.Metadata, Title: anthropic.String(in.Title), Budget: anthropic.BetaManagedAgentsBudgetLimitParam{MaxListCost: anthropic.BetaMonetaryAmountParam{Amount: strconv.FormatInt(in.BudgetCents, 10), Currency: "USD"}}, InitialEvents: []anthropic.BetaSessionNewParamsInitialEventUnion{{OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{OfText: &anthropic.BetaManagedAgentsTextBlockParam{Text: in.FirstMessage}}}}}}}
	if in.InputFileID != "" {
		params.Resources = []anthropic.BetaSessionNewParamsResourceUnion{{OfFile: &anthropic.BetaManagedAgentsFileResourceParams{
			FileID: in.InputFileID, MountPath: anthropic.String("/input.tar.gz"),
		}}}
	}
	v, err := a.sdk.Sessions.New(ctx, params)
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
	s.Usage = parseUsage(v.RawJSON())
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
func (a *API) Interrupt(ctx context.Context, id string) error {
	_, err := a.sdk.Sessions.Events.Send(ctx, id, anthropic.BetaSessionEventSendParams{Events: []anthropic.BetaManagedAgentsEventParamsUnion{{OfUserInterrupt: &anthropic.BetaManagedAgentsUserInterruptEventParams{}}}})
	err = apiError(err)
	if isNotFound(err) {
		return nil
	}
	return err
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
