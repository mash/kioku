package cli

import (
	"errors"
	"fmt"
)

func (a *App) accountStatus(args []string) error {
	if len(args) != 1 {
		return errors.New("unexpected account arguments")
	}
	var out map[string]any
	if _, err := a.request("GET", "/api/account", nil, &out); err != nil {
		return err
	}
	if app, ok := out["github_app"].(map[string]any); ok {
		if app["configured"] == false {
			fmt.Fprintln(a.Err, "GitHub App: 運用者の設定が必要です。")
		} else if app["enabled"] == false {
			fmt.Fprintln(a.Err, "GitHub App: 運用者が受付を停止しています。")
		} else if app["status"] == "reauthorization_required" {
			fmt.Fprintln(a.Err, "GitHub App: owner が kioku connect github で再接続してください。")
		}
	}
	return a.print(out)
}
