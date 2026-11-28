package cli

import (
	"errors"
	"flag"
	"fmt"

	"github.com/mash/kioku/internal/model"
)

// All pulls already have the credential selected by their outer command.
func (a *App) pull() error { return a.pullCommand(nil) }

func (a *App) pullCommand(args []string) (resultErr error) {
	f := flag.NewFlagSet("pull", flag.ContinueOnError)
	f.SetOutput(a.Err)
	a.descriptionFlag(f)
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: kioku [-n ORG/PROJECT] pull [--description TEXT]")
	}
	defer func() { a.recordSessionPull(resultErr) }()
	binding := a.Config.Namespace == "" && a.projectNamespace != ""
	result, err := a.syncProject()
	if binding && result.Namespace == "" && err != nil {
		return err
	}
	var outputErr error
	if !result.changed() && err == nil {
		_, outputErr = fmt.Fprintln(a.Out, "最新です")
	} else {
		outputErr = a.print(result)
	}
	return errors.Join(err, outputErr)
}

func (a *App) syncProject() (syncResult, error) {
	var list []model.ProjectEntry
	binding := a.Config.Namespace == "" && a.projectNamespace != ""
	if binding {
		var err error
		list, err = a.bindPullProject(a.projectNamespace)
		if err != nil {
			return syncResult{}, err
		}
	}
	result, err := a.syncWiki()
	if err != nil {
		return result, err
	}
	if !binding {
		list, err = a.fetchProjectList()
		if err != nil {
			if a.Err != nil {
				fmt.Fprintf(a.Err, "warning: could not refresh the General flag: %v\n", err)
			}
			return result, nil
		}
	}
	return result, a.projectEntryResult(a.updateGeneralFlags(list), "the next list or pull will refresh the General flag")
}

// Bind only a server-verified namespace, and never overwrite another binding.
func (a *App) bindPullProject(namespace string) ([]model.ProjectEntry, error) {
	unlock, err := a.localLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, err := a.readProjectConfig()
	if err != nil {
		return nil, err
	}
	if current.Server != "" && (current.Server != a.Config.Server || current.Namespace != namespace) {
		return nil, errors.New("working directory is already bound to a different project")
	}
	if err := a.checkUnboundMirror(); err != nil {
		return nil, err
	}
	a.Config.Namespace = namespace
	if _, err := a.projectPath(); err != nil {
		return nil, err
	}
	projects, err := a.fetchProjectList()
	if err != nil {
		return nil, err
	}
	for _, project := range projects {
		if project.Namespace == namespace {
			return projects, nil
		}
	}
	return nil, fmt.Errorf("project %s not found, or you are not a member", namespace)
}
