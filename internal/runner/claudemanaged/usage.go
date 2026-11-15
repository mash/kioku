package claudemanaged

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"

	"github.com/mash/kioku/internal/runner"
)

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
	model := usageObject(usageObject(root["agent"])["model"])
	out := runner.UsageSample{InputTokens: usageCount(u["input_tokens"]), OutputTokens: usageCount(u["output_tokens"]), CacheReadInputTokens: usageCount(u["cache_read_input_tokens"])}
	var id string
	if json.Unmarshal(model["id"], &id) == nil && id != "" {
		out.Model = &id
	}
	cache := usageObject(u["cache_creation"])
	out.CacheCreationInputTokens = runner.SumUsage([]runner.UsageSample{{CacheCreationInputTokens: usageCount(cache["ephemeral_5m_input_tokens"])}, {CacheCreationInputTokens: usageCount(cache["ephemeral_1h_input_tokens"])}}).CacheCreationInputTokens
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
			out.State = runner.StateUnknown
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
		}
	}
	if found == 0 {
		out.State = runner.StateNone
	}
	out.Usage = runner.SumUsage(samples)
	out.Cost = out.Usage.CostCents
	if len(ids) > 1 {
		out.Reason = "multiple Claude sessions for one attempt"
		out.Failed = true
	}
	return out, nil
}
