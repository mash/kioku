package cli

import (
	"errors"

	"github.com/mash/kioku/internal/model"
)

func (a *App) createProject(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: kioku project create <name>")
	}
	unlock, err := a.localLock()
	if err != nil {
		return err
	}
	defer unlock()
	current, err := a.readProjectConfig()
	if err != nil {
		return err
	}
	if current.Server != "" {
		return errors.New("working directory is already configured")
	}
	if err := a.preflightProjectStorage(); err != nil {
		return err
	}
	var p model.Project
	if _, err := a.request("POST", "/api/projects", map[string]string{"name": args[0]}, &p); err != nil {
		return err
	}
	return a.selectProject(p)
}
