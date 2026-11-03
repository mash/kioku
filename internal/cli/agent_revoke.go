package cli

import (
	"errors"
	"net/url"
)

func (a *App) agentRevoke(args []string, base string) error {
	var out any
	if len(args) != 2 || !safeID(args[1]) {
		return errors.New("usage: kioku agent revoke AGENT_ID")
	}
	if _, err := a.request("DELETE", base+"/"+url.PathEscape(args[1]), nil, &out); err != nil {
		return err
	}
	return a.print(out)
}
