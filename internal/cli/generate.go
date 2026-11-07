package cli

import (
	"errors"
	"flag"

	"github.com/mash/kioku/internal/model"
)

func (a *App) generate(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	flags.SetOutput(a.Err)
	wait := flags.Bool("wait", false, "wait for wiki generation and pull the revision")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: kioku generate [--wait]")
	}
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	var job model.Job
	if _, err = a.request("POST", p+"/generate", nil, &job); err != nil {
		return err
	}
	if *wait {
		return a.wait(job)
	}
	return a.print(job)
}
