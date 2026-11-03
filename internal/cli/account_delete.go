package cli

import "errors"

func (a *App) accountDelete(args []string) error {
	if len(args) != 2 || args[1] != "--yes" {
		return errors.New("usage: kioku account delete --yes")
	}
	return a.accountRequest("DELETE", "/api/account", map[string]string{"confirm": "delete-account"}, true)
}
