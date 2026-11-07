package cli

import "errors"

func (a *App) connect(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: kioku connect google|github")
	}
	return a.loopback(args[0], true)
}
