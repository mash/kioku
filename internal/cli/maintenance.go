package cli

import (
	"errors"
	"flag"
)

func (a *App) maintenance(args []string) error {
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	var out any
	f := flag.NewFlagSet("maintenance", flag.ContinueOnError)
	f.SetOutput(a.Err)
	keep := f.Int("keep", 1, "deprecated compatibility flag; page history is not retained")
	if err = f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: kioku maintenance")
	}
	_, err = a.request("POST", p+"/maintenance", map[string]int{"keep": *keep}, &out)

	if err != nil {
		return err
	}
	return a.print(out)
}
