package cli

import (
	"errors"
	"flag"

	"github.com/mash/kioku/internal/model"
)

func (a *App) createProject(args []string) error {
	f := flag.NewFlagSet("project create", flag.ContinueOnError)
	f.SetOutput(a.Err)
	a.descriptionFlag(f)
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || a.projectNamespace == "" {
		return errors.New("usage: kioku -n ORG/PROJECT project create [--description TEXT]")
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
	if _, err := a.request("POST", "/api/projects", map[string]string{"name": a.projectNamespace}, &p); err != nil {
		return err
	}
	return a.selectProject(p)
}
