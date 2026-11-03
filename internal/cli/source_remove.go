package cli

import (
	"errors"
	"flag"

	"github.com/mash/kioku/internal/model"
)

func (a *App) sourceRemove(args []string) error {
	f := flag.NewFlagSet("source remove", flag.ContinueOnError)
	f.SetOutput(a.Err)
	yes := f.Bool("yes", false, "confirm source deletion")
	wait := f.Bool("wait", false, "wait for source deletion (does not generate or pull wiki)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*yes || f.NArg() != 1 || !safeID(f.Arg(0)) {
		return errors.New("usage: kioku source remove --yes [--wait] SOURCE_ID")
	}
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	var job model.Job
	if _, err = a.request("DELETE", p+"/sources/"+f.Arg(0), nil, &job); err != nil {
		return err
	}
	if !*wait {
		return a.print(job)
	}
	return a.wait(job)
}
