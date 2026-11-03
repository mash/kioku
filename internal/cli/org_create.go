package cli

import "errors"

func (a *App) orgCreate(args []string) error {
	var out any
	if len(args) != 2 {
		return errors.New("usage: kioku org create <slug>")
	}
	_, err := a.request("POST", "/api/orgs", map[string]string{"slug": args[1]}, &out)
	if err != nil {
		return err
	}
	return a.print(out)
}
