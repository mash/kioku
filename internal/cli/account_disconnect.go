package cli

import "errors"

func (a *App) accountDisconnect(args []string) error {
	if len(args) != 2 || (args[1] != "github" && args[1] != "google") {
		return errors.New("usage: kioku account disconnect google|github")
	}
	return a.accountRequest("DELETE", "/api/account/connections/"+args[1], nil, false)
}
