package cli

import "errors"

func (a *App) agent(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kioku agent login CODE | list|revoke")
	}
	if args[0] == "login" {
		return a.agentLogin(args[1:])
	}
	if err := a.ensureProject(); err != nil {
		return err
	}
	base, _ := a.projectPath()
	base += "/agents"
	switch args[0] {
	case "list":
		return a.agentList(args, base)
	case "revoke":
		return a.agentRevoke(args, base)
	default:
		return errors.New("unknown agent command")
	}
}
