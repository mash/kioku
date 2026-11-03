package cli

import (
	"errors"
	"net/url"
)

func (a *App) accountRevokeSession(args []string) error {
	if len(args) != 2 || !safeID(args[1]) {
		return errors.New("usage: kioku account revoke-session SESSION_ID")
	}
	return a.accountRequest("DELETE", "/api/account/sessions/"+url.PathEscape(args[1]), nil, false)
}
