package cli

import (
	"errors"
	"flag"

	"github.com/mash/kioku/internal/model"
)

func (a *App) join(args []string) error {
	if a.projectNamespace != "" {
		return errors.New("-n cannot be used with project join; use cwd or -C")
	}
	f := flag.NewFlagSet("project join", flag.ContinueOnError)
	f.SetOutput(a.Err)
	a.descriptionFlag(f)
	if err := f.Parse(args); err != nil {
		return err
	}
	args = f.Args()
	if len(args) != 1 {
		return errors.New("usage: kioku project join [--description TEXT] <invite-url>")
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
	if _, err := a.request("POST", "/api/join", map[string]string{"token": args[0]}, &p); err != nil {
		return err
	}
	return a.selectProject(p)
}
