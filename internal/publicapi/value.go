// Package publicapi separates public responses from persisted server models.
package publicapi

import (
	"strings"

	"github.com/mash/kioku/internal/model"
)

func Value(v any) any {
	switch x := v.(type) {
	case *model.Project:
		if x == nil {
			return nil
		}
		return Value(*x)
	case *model.ProjectEntry:
		if x == nil {
			return nil
		}
		return Value(*x)
	case *model.Job:
		if x == nil {
			return nil
		}
		return Value(*x)
	case *model.Audit:
		if x == nil {
			return nil
		}
		return Value(*x)
	case model.Project:
		return map[string]any{"namespace": x.Namespace, "slug": x.Slug, "name": x.Name, "members": x.Members, "revision": x.Revision, "created": x.Created, "wiki_incomplete": x.WikiIncomplete, "wiki_updated_at": x.WikiUpdatedAt, "next_generate_at": x.NextGenerateAt, "generate_interval_seconds": x.GenerateIntervalSeconds}
	case model.ProjectEntry:
		out := Value(x.Project).(map[string]any)
		out["inbox"] = x.Inbox
		out["inbox_count"] = x.InboxCount
		return out
	case model.Job:
		message := x.Error
		if x.ProjectID != "" {
			message = strings.ReplaceAll(message, x.ProjectID, "[project]")
		}
		out := map[string]any{"id": x.ID, "kind": x.Kind, "status": x.Status, "error": message, "revision": x.Revision, "created": x.Created, "unchanged": x.Unchanged, "automatic": x.Automatic, "agent_budget_cents": x.AgentBudgetCents, "user_id": x.UserID, "agent_id": x.AgentID}
		if x.Run != nil {
			out["run"] = map[string]any{"phase": x.Run.Phase, "cost_cents": x.Run.CostCents, "budget_cents": x.Run.BudgetCents, "prepared_at": x.Run.PreparedAt, "deadline": x.Run.Deadline, "cleanup_complete": x.Run.CleanupComplete}
		}
		return out
	case model.Audit:
		return map[string]any{"id": x.ID, "user_id": x.UserID, "action": x.Action, "detail": x.Detail, "created": x.Created}
	case []model.Project:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = Value(v)
		}
		return out
	case []model.ProjectEntry:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = Value(v)
		}
		return out
	case []model.Job:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = Value(v)
		}
		return out
	case []model.Audit:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = Value(v)
		}
		return out
	default:
		return v
	}
}
