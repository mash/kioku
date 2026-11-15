package claudemanaged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
)

const MultipleSessionsReason = "multiple Claude sessions for one attempt"

func usageObject(raw json.RawMessage) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	_ = json.Unmarshal(raw, &out)
	return out
}
func usageCount(raw json.RawMessage) *int64 {
	var n *int64
	if json.Unmarshal(raw, &n) != nil || n == nil || *n < 0 {
		return nil
	}
	return n
}
func parseUsage(raw string) runner.UsageSample {
	root := usageObject(json.RawMessage(raw))
	u := usageObject(root["usage"])
	agentModel := usageObject(usageObject(root["agent"])["model"])
	out := runner.UsageSample{InputTokens: usageCount(u["input_tokens"]), OutputTokens: usageCount(u["output_tokens"]), CacheReadInputTokens: usageCount(u["cache_read_input_tokens"])}
	var id string
	if json.Unmarshal(agentModel["id"], &id) == nil && id != "" {
		out.Model = &id
	}
	cache := usageObject(u["cache_creation"])
	out.CacheCreationInputTokens = model.SumMeasurements(usageCount(cache["ephemeral_5m_input_tokens"]), usageCount(cache["ephemeral_1h_input_tokens"]))
	cost := usageObject(u["list_cost"])
	var currency, amount string
	if json.Unmarshal(cost["currency"], &currency) == nil && currency == "USD" && json.Unmarshal(cost["amount"], &amount) == nil {
		if cents, err := strconv.ParseInt(amount, 10, 64); err == nil && cents >= 0 {
			out.CostCents = &cents
		}
	}
	return out
}

func discoverSessions(ctx context.Context, client Client, a runner.Attempt) ([]string, error) {
	ids, err := client.FindAttemptSessions(ctx, a.Key)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []string{}
	for _, id := range append(ids, a.Run.ID) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Cleanup discovers again after usage was recorded. Any newly visible session
// must stop before its resources disappear and reconciliation loses its ID.
func confirmCleanupStopped(ctx context.Context, client Client, a runner.Attempt, ids []string, file bool) error {
	status, err := inspectSessions(ctx, client, a, ids, file)
	if err != nil {
		return err
	}
	if status.State != runner.StateStopped && status.State != runner.StateNone {
		return fmt.Errorf("%w: Claude cleanup is waiting for all sessions to stop", runner.ErrTemporary)
	}
	return nil
}

func inspectSessions(ctx context.Context, client Client, a runner.Attempt, ids []string, file bool) (runner.Status, error) {
	out := runner.Status{Run: a.Run, State: runner.StateNone}
	if len(ids) == 0 {
		return out, nil
	}
	if out.Run.ID == "" {
		out.Run.ID = ids[0]
	}
	out.State = runner.StateStopped
	samples := []runner.UsageSample{}
	found := 0
	for _, id := range ids {
		s, err := client.GetSession(ctx, id)
		if isNotFound(err) {
			// A previous cleanup pass may already have deleted this session.
			// Its usage is unknown, but its absence does not keep other runs alive.
			samples = append(samples, runner.UsageSample{})
			continue
		}
		if err != nil {
			return runner.Status{}, err
		}
		if s.ID != id || s.Metadata["kioku_job_id"] != a.Key.JobID || s.Metadata["kioku_attempt_id"] != a.Key.AttemptID {
			return runner.Status{}, errors.New("Claude session does not match the generation attempt")
		}
		found++
		samples = append(samples, s.Usage)
		state := runner.StateUnknown
		reason := ""
		switch s.Status {
		case "running", "rescheduling":
			state = runner.StateRunning
		case "terminated":
			state = runner.StateStopped
			reason = "session terminated"
			if file {
				reason = "Claude session terminated"
			}
		case "idle":
			state = runner.StateStopped
			if file {
				switch s.LastStopReason {
				case "end_turn":
				case "":
					state = runner.StateRunning
				default:
					reason = "Claude session did not finish successfully"
				}
			} else {
				switch s.LastStopReason {
				case "retries_exhausted":
					reason = "agent gave up after errors"
				case "requires_action":
					reason = "agent requires action"
				}
			}
		}
		if state == runner.StateUnknown {
			out.State = runner.StateUnknown
		} else if state == runner.StateRunning && out.State != runner.StateUnknown {
			out.State = runner.StateRunning
		}
		if reason != "" {
			out.Reason = reason
			out.Failed = true
		}
	}
	if found == 0 {
		out.State = runner.StateNone
	}
	out.Usage = runner.SumUsage(samples)
	if len(ids) > 1 {
		out.Reason = MultipleSessionsReason
		out.Failed = true
	}
	return out, nil
}
