package cli

import "fmt"

func (a *App) project(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kioku project create|join|list|delete|share|usage|audit|maintenance")
	}
	switch args[0] {
	case "create":
		return a.createProject(args[1:])
	case "join":
		return a.join(args[1:])
	case "list":
		return a.list()
	case "delete":
		return a.deleteProject(args[1:])
	case "share":
		return a.share(args[1:])
	case "usage":
		return a.usage()
	case "audit":
		return a.audit()
	case "maintenance":
		return a.maintenance(args[1:])
	default:
		return fmt.Errorf("unknown project command %q", args[0])
	}
}
