package cli

import "errors"

func (a *App) agentList(args []string, base string) error {
	var out any
	if len(args) != 1 {
		return errors.New("usage: kioku agent list")
	}
	if _, err := a.request("GET", base, nil, &out); err != nil {
		return err
	}
	return a.print(out)
}
