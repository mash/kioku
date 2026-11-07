package cli

import (
	"errors"
	"fmt"
)

func (a *App) deleteProject(args []string) error {
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	var out any
	if len(args) != 1 || args[0] != "--yes" {
		return errors.New("usage: kioku project delete --yes (permanently deletes project and cloud objects)")
	}
	_, err = a.request("DELETE", p, nil, &out)
	if err == nil {
		fmt.Fprintln(a.Out, "ローカルの取得済みコピーは保持しています。不要なら手動で削除してください。")
	}

	if err != nil {
		return err
	}
	return a.print(out)
}
