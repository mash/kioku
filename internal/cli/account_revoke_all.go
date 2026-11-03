package cli

import "errors"

func (a *App) accountRevokeAll(args []string) error {
	if len(args) != 2 || args[1] != "--yes" {
		return errors.New("usage: kioku account revoke-all --yes (also revokes agent credentials)")
	}
	return a.accountRequest("POST", "/api/account/revoke-all", nil, true)
}
