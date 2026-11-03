package cli

import (
	"errors"
	"flag"
)

func (a *App) doctor(args []string) error {
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("doctor", flag.ContinueOnError)
	f.SetOutput(a.Err)
	llm := f.Bool("llm", false, "check Managed Agents configuration without an API call")
	if err = f.Parse(args); err != nil {
		return err
	}
	var report struct {
		OK     bool              `json:"ok"`
		Checks map[string]string `json:"checks"`
	}
	if _, err = a.request("POST", p+"/doctor", map[string]bool{"llm": *llm}, &report); err != nil {
		return err
	}
	if err = a.print(report); err != nil {
		return err
	}
	if !report.OK {
		return errors.New("some connections need configuration; see checks above")
	}
	return nil
}
