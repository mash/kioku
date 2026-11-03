package cli

import "errors"

func (a *App) shareTransfer(args []string, p string) error {
	var v any
	if len(args) != 3 || args[1] != "--yes" {
		return errors.New("usage: kioku share transfer --yes ACCOUNT_ID_OR_EMAIL")
	}
	_, err := a.request("POST", p+"/owner", map[string]string{"target": args[2]}, &v)
	if err != nil {
		return err
	}
	return a.print(v)
}
