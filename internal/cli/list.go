package cli

import "github.com/mash/kioku/internal/model"

func (a *App) fetchProjectList() ([]model.ProjectEntry, error) {
	var list []model.ProjectEntry
	_, err := a.request("GET", "/api/projects", nil, &list)
	return list, err
}

func (a *App) list() error {
	list, err := a.fetchProjectList()
	if err != nil {
		return err
	}
	if err := a.projectEntryResult(a.updateGeneralFlags(list), "the next project list will refresh the General flag"); err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		entry := map[string]any{"namespace": p.Namespace, "name": p.Name}
		if p.General {
			entry["general"] = true
		}
		out = append(out, entry)
	}
	return a.print(out)
}
