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
	if _, err := browserOrigin(a.Config.Server, target); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, target)
	if err := openBrowser(a.Config.Server, target); err != nil {
		fmt.Fprintf(a.Err, "ブラウザを開けませんでした。表示された URL をブラウザに貼り付けてください: %v\n", err)
	}
	return nil
}
