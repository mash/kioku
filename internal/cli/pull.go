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
	if a.Config.Namespace != "" || a.projectNamespace == "" {
		return a.syncWiki()
	}
	project, err := a.bindPullProject(a.projectNamespace)
	if err != nil {
		return syncResult{}, err
	}
	result, err := a.syncWiki()
	if err != nil {
		return result, err
	}
	return result, a.projectEntryResult(a.updateGeneralFlags([]model.ProjectEntry{project}), "the next project list will refresh the General flag")
}

// Bind only a server-verified namespace, and never overwrite another binding.
func (a *App) bindPullProject(namespace string) (model.ProjectEntry, error) {
	unlock, err := a.localLock()
	if err != nil {
		return model.ProjectEntry{}, err
	}
	defer unlock()
	current, err := a.readProjectConfig()
	if err != nil {
		return model.ProjectEntry{}, err
	}
	if current.Server != "" && (current.Server != a.Config.Server || current.Namespace != namespace) {
		return model.ProjectEntry{}, errors.New("working directory is already bound to a different project")
	}
	if err := a.checkUnboundMirror(); err != nil {
		return model.ProjectEntry{}, err
	}
	a.Config.Namespace = namespace
	if _, err := a.projectPath(); err != nil {
		return model.ProjectEntry{}, err
	}
	projects, err := a.fetchProjectList()
	if err != nil {
		return model.ProjectEntry{}, err
	}
	for _, project := range projects {
		if project.Namespace == namespace {
			return project, nil
		}
	}
	return model.ProjectEntry{}, fmt.Errorf("project %s not found, or you are not a member", namespace)
}
