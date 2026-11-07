package cli

import (
	"errors"
	"net/url"
)

func (a *App) shareRemove(args []string, p string) error {
	var v any
	if len(args) != 2 {
		return errors.New("usage: kioku project share remove <email-or-id>")
	}
	_, err := a.request("DELETE", p+"/members/"+url.PathEscape(args[1]), nil, &v)
	if err != nil {
		return err
	}
	return a.print(v)
}
