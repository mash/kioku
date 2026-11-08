package cli

import (
	"errors"

	"github.com/mash/kioku/internal/model"
)

func (a *App) status() error {
	if err := a.ensureProject(); err != nil {
		return err
	}
	result := map[string]any{"namespace": a.Config.Namespace, "server": a.Config.Server}
	var projects []model.ProjectEntry
	if _, err := a.request("GET", "/api/projects", nil, &projects); err != nil {
		return err
	}
	found := false
	for _, p := range projects {
		if p.Namespace == a.Config.Namespace {
			result["inbox"] = p.Inbox
			result["inbox_count"] = p.InboxCount
			result["remote_revision"] = p.Revision
			result["wiki_updated_at"] = p.WikiUpdatedAt
			result["wiki_incomplete"] = p.WikiIncomplete
			result["next_generate_at"] = p.NextGenerateAt
			found = true
		}
	}
	if !found {
		return errors.New("project no longer accessible")
	}
	return a.print(result)
}
