package cli

import (
	"errors"
	"fmt"
)

func (a *App) account(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kioku account status|sessions|revoke-session|revoke-all|unlink|delete")
	}
	switch args[0] {
	case "status":
		return a.accountStatus(args)
	case "sessions":
		return a.accountSessions(args)
	case "revoke-session":
		return a.accountRevokeSession(args)
	case "revoke-all":
		return a.accountRevokeAll(args)
	case "unlink":
		return a.accountUnlink(args)
	case "delete":
		return a.accountDelete(args)
	default:
		return fmt.Errorf("unknown account command %q", args[0])
	}
}

func (a *App) accountRequest(method, path string, input any, clear bool) error {
	var out any
	if _, err := a.request(method, path, input, &out); err != nil {
		return err
	}
	if clear {
		a.Config.Token = ""
		if err := a.saveCredentials(); err != nil {
			return err
		}
	}
	return a.print(out)
}
