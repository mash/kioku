package cli

import "errors"

func (a *App) share(args []string) error {
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: kioku project share list|invite [email]|remove <email-or-id>")
	}
	switch args[0] {
	case "transfer":
		return a.shareTransfer(args, p)
	case "list":
		return a.shareList(args, p)
	case "invite":
		return a.shareInvite(args, p)
	case "remove":
		return a.shareRemove(args, p)
	default:
		return errors.New("unknown share command")
	}
}
