package cli

import (
	"github.com/mash/kioku/internal/model"
)

func (a *App) list() error {
	var v []model.Project
	if _, err := a.request("GET", "/api/projects", nil, &v); err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(v))
	for _, p := range v {
		out = append(out, map[string]any{"namespace": p.Namespace, "name": p.Name})
	}
	return a.print(out)
}
