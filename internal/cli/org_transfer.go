package cli

import (
	"errors"
	"net/url"
)

func (a *App) orgChangeOwner(args []string) error {
	var out any
	if len(args) != 4 || args[1] != "--yes" || !safeSlug(args[2]) {
		return errors.New("usage: kioku org " + args[0] + " --yes ORG ACCOUNT_ID_OR_EMAIL")
	}
	suffix := "/owner"
	if args[0] == "add-owner" {
		suffix = "/owners"
	}
	if _, err := a.request("POST", "/api/orgs/"+url.PathEscape(args[2])+suffix, map[string]string{"target": args[3]}, &out); err != nil {
		return err
	}
	return a.print(out)
}
