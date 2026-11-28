package cli

import (
	"errors"
	"time"

	"github.com/mash/kioku/internal/model"
)

func (a *App) status() error {
	if err := a.ensureProject(); err != nil {
		return err
	}
	result := map[string]any{"namespace": a.Config.Namespace, "server": a.Config.Server}
	rows, err := readQueue(a.queuePath(), true)
	if err != nil {
		return err
	}
	lock, err := readCaptureLock(a.captureLockPath())
	if err != nil {
		return err
	}
	result["queue"], result["capture"] = rows, nil
	if lockBusy(lock, time.Now()) {
		result["capture"] = map[string]any{"id": lock.BgID, "started_at": lock.StartedAt}
	}
	if reason := a.lastCaptureLaunchError(); reason != "" {
		result["last_capture_launch_error"] = reason
	}
	serverError := func(err error) error {
		result["server_error"] = err.Error()
		return errors.Join(err, a.print(result))
	}
	if err := a.selectCredential(false); err != nil {
		return serverError(err)
	}
	var projects []model.ProjectEntry
	if _, err := a.request("GET", "/api/projects", nil, &projects); err != nil {
		return serverError(err)
	}
	found := false
	for _, p := range projects {
		if p.Namespace == a.Config.Namespace {
			result["inbox"] = p.Inbox
			result["inbox_count"] = p.InboxCount
			result["remote_revision"] = p.Revision
			result["wiki_updated_at"] = p.WikiUpdatedAt
			found = true
		}
	}
	if !found {
		return serverError(errors.New("project no longer accessible"))
	}
	return a.print(result)
}
