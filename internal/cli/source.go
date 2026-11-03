package cli

import "errors"

func (a *App) source(args []string) error {
	if len(args) == 0 || args[0] != "remove" {
		return errors.New("usage: kioku source remove --yes [--wait] SOURCE_ID; use kioku put to save sources and kioku open to view them")
	}
	return a.sourceRemove(args)
}
