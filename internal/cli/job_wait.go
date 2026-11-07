package cli

import (
	"fmt"
	"time"

	"github.com/mash/kioku/internal/model"
)

func (a *App) jobWait(path string) error { return a.jobStatus(path, true) }

func (a *App) wait(j model.Job) error {
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "job %s\n", j.ID)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if j.Status == "completed" {
			if j.Unchanged {
				fmt.Fprintln(a.Out, "No waiting inbox items; wiki is up to date.")
			}
			if j.Unchanged && j.Revision == "" {
				return a.print(j)
			}
			return a.pull()
		}
		if j.Status == "failed" {
			return fmt.Errorf("job failed: %s; retry with kioku job retry %s", j.Error, j.ID)
		}
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()
		case <-ticker.C:
			if _, err = a.request("GET", p+"/jobs/"+j.ID, nil, &j); err != nil {
				return err
			}
		}
	}
}
