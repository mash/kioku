package cli

import "errors"

func (a *App) accountSessions(args []string) error {
	if len(args) != 1 {
		return errors.New("unexpected account arguments")
	}
	return a.accountRequest("GET", "/api/account", nil, false)
}
