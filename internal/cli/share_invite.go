package cli

import "errors"

func (a *App) shareInvite(args []string, p string) error {
	var v any
	email := ""
	if len(args) > 2 {
		return errors.New("usage: kioku project share invite [email]")
	}
	if len(args) == 2 {
		email = args[1]
	}
	_, err := a.request("POST", p+"/invites", map[string]string{"email": email}, &v)
	if err != nil {
		return err
	}
	return a.print(v)
}
