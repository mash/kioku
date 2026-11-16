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
	res, err := r.send(ctx, method, path, input, "application/json")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out); err != nil {
			return fmt.Errorf("%w: invalid OpenAI response", runner.ErrTemporary)
		}
	}
	return nil
}
func (r *Runner) send(ctx context.Context, method, path string, input io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.config.BaseURL, "/")+path, input)
	if err != nil {
		return nil, errors.New("invalid OpenAI API endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+r.config.APIKey)
	req.Header.Set("OpenAI-Beta", "agents=v1")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: OpenAI request failed", runner.ErrTemporary)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		res.Body.Close()
		return nil, apiError(res.StatusCode)
	}
	return res, nil
}
func metadata(k runner.AttemptKey) map[string]string {
	return map[string]string{"kioku_job_id": k.JobID, "kioku_attempt_id": k.JobID}
}

type resource struct {
	ID          string            `json:"id"`
	Metadata    map[string]string `json:"metadata"`
	Status      string            `json:"status"`
	VaultIDs    []string          `json:"vault_ids"`
	Environment struct {
		Type    string `json:"type"`
		Network struct {
			Access string `json:"access"`
		} `json:"network"`
	} `json:"environment"`
}

// Scan unfiltered pages and match all identity fields locally: metadata indexes
// are eventually consistent. Never replay a session creation after an ambiguous response.
func (r *Runner) find(ctx context.Context, path string, k runner.AttemptKey) ([]resource, error) {
	if k.JobID == "" || k.ProjectID == "" {
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
	for key, value := range metadata(a.Key) {
		if s.Metadata[key] != value {
			return runner.Status{}, errors.New("OpenAI session does not match the generation attempt")
		}
	}
	a.Run.ID = s.ID
	out := runner.Status{Run: a.Run}
	if !s.offline() {
		const reason = "OpenAI session does not report network access disabled"
		if s.Status == "idle" || s.Status == "failed" {
			out.State, out.Reason = runner.StateStopped, reason
			return out, nil
		}
		return runner.Status{}, errors.New(reason)
	}
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
			HasMore bool `json:"has_more"`
			Data    []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"data"`
		}
		if err = r.request(ctx, "GET", "/agents/sessions/"+url.PathEscape(s.ID)+"/turns?order=desc&limit=2", nil, &turns); err != nil {
			return runner.Status{}, err
		}
		if len(turns.Data) == 0 {
			out.State = runner.StateRunning
			break
		}
		if turns.HasMore || len(turns.Data) != 1 || turns.Data[0].ID == "" {
			out.State, out.Reason = runner.StateStopped, "OpenAI session has ambiguous turn identity"
			break
		}
		out.TurnID = turns.Data[0].ID
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
