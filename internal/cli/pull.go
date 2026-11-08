package cli

import (
	"errors"
	"flag"
	"fmt"

	"github.com/mash/kioku/internal/model"
)

func (a *App) pull() error { return a.pullCommand(nil) }

func (a *App) pullCommand(args []string) error {
	f := flag.NewFlagSet("pull", flag.ContinueOnError)
	f.SetOutput(a.Err)
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() > 1 {
		return errors.New("usage: kioku pull [organization/project]")
	}
	if f.NArg() == 1 {
		namespace := f.Arg(0)
		if !validNamespace(namespace) {
			return errors.New("namespace must be organization/project")
		}
		if err := a.bindPullProject(namespace); err != nil {
			return err
		}
	}
	result, err := a.syncWiki()
	var outputErr error
	if !result.changed() && err == nil {
		_, outputErr = fmt.Fprintln(a.Out, "最新です")
	} else {
		outputErr = a.print(result)
	}
	return errors.Join(err, outputErr)
}

// Bind only a server-verified namespace, and never overwrite another binding.
func (a *App) bindPullProject(namespace string) error {
	unlock, err := a.localLock()
	if err != nil {
		return err
	}
	defer unlock()
	current, err := a.readProjectConfig()
	if err != nil {
		return err
	}
	if current.Server != "" && (current.Server != a.Config.Server || current.Namespace != namespace) {
		return errors.New("working directory is already bound to a different project")
	}
	a.Config.Namespace = namespace
	if _, err := a.projectPath(); err != nil {
		return err
	}
	var projects []model.Project
	if _, err := a.request("GET", "/api/projects", nil, &projects); err != nil {
		return err
	}
	for _, project := range projects {
		if project.Namespace == namespace {
			return a.saveProjectConfig()
		}
	}
	return errSyncForbidden
}
