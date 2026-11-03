package cli

import "errors"

func (a *App) accountUnlink(args []string) error {
	if len(args) != 3 || args[1] != "--yes" || (args[2] != "google" && args[2] != "github") {
		return errors.New("usage: kioku account unlink --yes google|github")
	}
	return a.accountRequest("DELETE", "/api/account/identities/"+args[2], nil, false)
}
