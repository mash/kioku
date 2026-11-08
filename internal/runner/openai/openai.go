// Package openai implements the OpenAI-hosted Agents API generation lifecycle.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mash/kioku/internal/runner"
)

type Config struct {
	APIKey, Model, Prompt string
	// BaseURL and HTTPClient allow an isolated provider endpoint in tests.
	BaseURL    string
	HTTPClient *http.Client
}
type Runner struct {
	config Config
	client *http.Client
}

var _ runner.Runner = (*Runner)(nil)

func New(c Config) *Runner {
	if c.BaseURL == "" {
		c.BaseURL = "https://api.openai.com/v1"
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if c.HTTPClient != nil {
		copy := *c.HTTPClient
		client = &copy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Runner{config: c, client: client}
}

type apiError int

func (e apiError) Error() string { return fmt.Sprintf("OpenAI Agents API: HTTP %d", int(e)) }
func (e apiError) Is(target error) bool {
	return target == runner.ErrTemporary && (e == 409 || e == 429 || e >= 500)
}
func (r *Runner) request(ctx context.Context, method, path string, body, out any) error {
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return errors.New("OpenAI request encoding failed")
		}
		input = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.config.BaseURL, "/")+path, input)
	if err != nil {
		return errors.New("invalid OpenAI API endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+r.config.APIKey)
	req.Header.Set("OpenAI-Beta", "agents=v1")
	req.Header.Set("Content-Type", "application/json")
	res, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: OpenAI request failed", runner.ErrTemporary)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return apiError(res.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out); err != nil {
			return fmt.Errorf("%w: invalid OpenAI response", runner.ErrTemporary)
		}
	}
	return nil
}
func metadata(k runner.AttemptKey) map[string]string {
	return map[string]string{"kioku_job_id": k.JobID, "kioku_attempt_id": k.AttemptID}
}

type resource struct {
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`
	Status   string            `json:"status"`
	VaultIDs []string          `json:"vault_ids"`
}

func ref(s resource) runner.RunRef {
	resources := map[string]string{"provider": "openai"}
	if len(s.VaultIDs) > 0 {
		resources["vault_id"] = s.VaultIDs[0]
	}
	return runner.RunRef{ID: s.ID, Resources: resources}
}
func endpoint(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443" && u.Port() != "8443") {
		return "", errors.New("OpenAI runner requires HTTPS job URLs on port 443 or 8443")
	}
	return u.Host, nil
}
func (r *Runner) Start(ctx context.Context, spec runner.RunSpec) (runner.RunRef, error) {
	empty := runner.RunRef{Resources: map[string]string{"provider": "openai"}}
	host, err := endpoint(spec.InputURL)
	if err != nil {
		return empty, err
	}
	other, err := endpoint(spec.OutputURL)
	if err != nil || host != other {
		return empty, errors.New("OpenAI job URLs must have the same HTTPS authority")
	}
	if spec.Key.JobID == "" || spec.Key.AttemptID == "" || spec.Key.ProjectID == "" || spec.Token == "" || spec.BudgetCents < 1 || r.config.APIKey == "" || r.config.Model == "" || strings.TrimSpace(r.config.Prompt) == "" {
		return empty, errors.New("invalid OpenAI runner configuration or job")
	}
	if !spec.Deadline.IsZero() && !time.Now().Before(spec.Deadline) {
		return empty, errors.New("OpenAI job deadline exceeded")
	}
	u, _ := url.Parse(spec.InputURL)
	var vault resource
	err = r.request(ctx, "POST", "/vaults", map[string]any{"name": "Kioku " + spec.Key.JobID, "metadata": metadata(spec.Key)}, &vault)
	if err != nil {
		return empty, err
	}
	if vault.ID == "" {
		return empty, fmt.Errorf("%w: missing OpenAI vault ID", runner.ErrTemporary)
	}
	empty.Resources["vault_id"] = vault.ID
	credentials, err := runner.VaultCredentials(spec, u.Hostname())
	if err != nil {
		return empty, err
	}
	for _, c := range credentials {
		err = r.request(ctx, "POST", "/vaults/"+url.PathEscape(vault.ID)+"/credentials", map[string]any{"name": c.Name, "auth": map[string]any{"type": "environment_variable", "secret_name": c.Name, "secret_value": string(c.Value), "networking": map[string]any{"type": "limited", "allowed_hosts": c.AllowedHosts}, "injection_location": map[string]bool{"header": true}}}, nil)
		if err != nil {
			return empty, err
		}
	}
	var session resource
	err = r.request(ctx, "POST", "/agents/sessions", map[string]any{
		"agent":       map[string]any{"model": r.config.Model, "instructions": r.config.Prompt, "multi_agent": map[string]bool{"enabled": false}, "tools": []any{}},
		"environment": map[string]any{"type": "openai_hosted", "container_size": "small"},
		"metadata":    metadata(spec.Key), "vault_ids": []string{vault.ID}, "input": spec.FirstMessage, "spend_control": map[string]int64{"limit": spec.BudgetCents}, "stream": false,
	}, &session)
	if err != nil {
		return empty, err
	}
	if session.ID == "" {
		return empty, fmt.Errorf("%w: missing OpenAI session ID", runner.ErrTemporary)
	}
	empty.ID = session.ID
	return empty, nil
}

// Scan unfiltered pages and match all identity fields locally: metadata indexes
// are eventually consistent. Never replay a session creation after an ambiguous response.
func (r *Runner) find(ctx context.Context, path string, k runner.AttemptKey) ([]resource, error) {
	if k.JobID == "" || k.AttemptID == "" || k.ProjectID == "" {
		return nil, errors.New("missing OpenAI attempt identity")
	}
	var found []resource
	after := ""
	seen := map[string]bool{}
	for {
		q := url.Values{"limit": {"100"}, "order": {"asc"}}
		if after != "" {
			q.Set("after", after)
		}
		var page struct {
			Data    []resource `json:"data"`
			HasMore bool       `json:"has_more"`
			LastID  string     `json:"last_id"`
		}
		if err := r.request(ctx, "GET", path+"?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, v := range page.Data {
			match := v.ID != ""
			for key, value := range metadata(k) {
				if v.Metadata[key] != value {
					match = false
				}
			}
			if match {
				found = append(found, v)
			}
		}
		if !page.HasMore {
			return found, nil
		}
		if page.LastID == "" || seen[page.LastID] {
			return nil, fmt.Errorf("%w: invalid OpenAI pagination", runner.ErrTemporary)
		}
		after = page.LastID
		seen[after] = true
	}
}
func (r *Runner) sessions(ctx context.Context, a runner.Attempt) ([]resource, error) {
	if a.Run.ID != "" {
		return []resource{{ID: a.Run.ID}}, nil
	}
	return r.find(ctx, "/agents/sessions", a.Key)
}
func (r *Runner) Inspect(ctx context.Context, a runner.Attempt) (runner.Status, error) {
	sessions, err := r.sessions(ctx, a)
	if err != nil {
		return runner.Status{}, err
	}
	if len(sessions) == 0 {
		return runner.Status{State: runner.StateNone}, nil
	}
	if len(sessions) != 1 {
		return runner.Status{}, errors.New("multiple OpenAI sessions for one attempt")
	}
	var s resource
	if err = r.request(ctx, "GET", "/agents/sessions/"+url.PathEscape(sessions[0].ID), nil, &s); err != nil {
		if errors.Is(err, apiError(404)) {
			return runner.Status{Run: a.Run, State: runner.StateNone}, nil
		}
		return runner.Status{}, err
	}
	if s.ID == "" || s.ID != sessions[0].ID {
		return runner.Status{}, fmt.Errorf("%w: invalid OpenAI session identity", runner.ErrTemporary)
	}
	out := runner.Status{Run: ref(s)}
	// spend_control.consumed is best-effort, rounded down, and may exclude charges.
	// Do not publish it as the runner's settled cost.
	switch s.Status {
	case "in_progress":
		out.State = runner.StateRunning
	case "failed":
		out.State = runner.StateStopped
		out.Reason = "OpenAI session failed"
	case "requires_action":
		out.State = runner.StateRunning
		out.Reason = "OpenAI session requires action"
	case "idle":
		var turns struct {
			Data []struct {
				Status string `json:"status"`
			} `json:"data"`
		}
		if err = r.request(ctx, "GET", "/agents/sessions/"+url.PathEscape(s.ID)+"/turns?order=desc&limit=1", nil, &turns); err != nil {
			return runner.Status{}, err
		}
		if len(turns.Data) == 0 {
			out.State = runner.StateRunning
			break
		}
		switch turns.Data[0].Status {
		case "completed":
			out.State = runner.StateStopped
		case "failed":
			out.State = runner.StateStopped
			out.Reason = "OpenAI turn failed"
		case "cancelled":
			out.State = runner.StateStopped
			out.Reason = "OpenAI turn cancelled"
		default:
			out.State = runner.StateRunning
		}
	}
	return out, nil
}
func (r *Runner) Stop(ctx context.Context, a runner.Attempt) error {
	sessions, err := r.sessions(ctx, a)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range sessions {
		err := r.request(ctx, "POST", "/agents/sessions/"+url.PathEscape(s.ID)+"/events", map[string]any{"events": []any{map[string]string{"type": "agent.session.input.cancel"}}}, nil)
		if !errors.Is(err, apiError(404)) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
func (r *Runner) Cleanup(ctx context.Context, a runner.Attempt) error {
	// Discover all resources, including partial starts and duplicate sessions.
	sessions, err := r.find(ctx, "/agents/sessions", a.Key)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	if a.Run.ID != "" {
		ids[a.Run.ID] = true
	}
	for _, s := range sessions {
		ids[s.ID] = true
	}
	for id := range ids {
		if err = r.request(ctx, "DELETE", "/agents/sessions/"+url.PathEscape(id), nil, nil); err != nil && !errors.Is(err, apiError(404)) {
			return err
		}
	}
	vaults, err := r.find(ctx, "/vaults", a.Key)
	if err != nil {
		return err
	}
	ids = map[string]bool{}
	if id := a.Run.Resources["vault_id"]; id != "" {
		ids[id] = true
	}
	for _, v := range vaults {
		ids[v.ID] = true
	}
	for id := range ids {
		if err = r.request(ctx, "DELETE", "/vaults/"+url.PathEscape(id), nil, nil); err != nil && !errors.Is(err, apiError(404)) {
			return err
		}
	}
	return nil
}
