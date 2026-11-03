package cli

import (
	"fmt"
	"strings"
)

func (a *App) open() error {
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	target := a.Config.Server + strings.TrimPrefix(p, "/api")
	if a.Config.Namespace != "" {
		target = a.Config.Server + "/" + a.Config.Namespace
	}
	fmt.Fprintln(a.Out, target)
	openBrowser(target)
	return nil
}
