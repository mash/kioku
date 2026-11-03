package cli

import "github.com/mash/kioku/internal/model"

func (a *App) jobStatus(path string, wait bool) error {
	var job model.Job
	if _, err := a.request("GET", path, nil, &job); err != nil {
		return err
	}
	if wait {
		return a.wait(job)
	}
	return a.print(job)
}
