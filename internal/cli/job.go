package cli

import (
	"errors"
	"flag"
)

func (a *App) job(args []string) error {
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: kioku job status|wait JOB_ID | kioku job retry [--wait] JOB_ID")
	}
	f := flag.NewFlagSet("job "+args[0], flag.ContinueOnError)
	f.SetOutput(a.Err)
	wait := f.Bool("wait", false, "wait for completion; pull only generation results")
	if err = f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 1 || !safeID(f.Arg(0)) {
		return errors.New("usage: kioku job status|wait JOB_ID | kioku job retry [--wait] JOB_ID")
	}
	switch args[0] {
	case "status":
		return a.jobStatus(p+"/jobs/"+f.Arg(0), *wait)
	case "wait":
		return a.jobWait(p + "/jobs/" + f.Arg(0))
	case "retry":
		return a.jobRetry(p+"/jobs/"+f.Arg(0), *wait)
	default:
		return errors.New("unknown job command")
	}
}
