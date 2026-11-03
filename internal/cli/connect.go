package cli

import (
	"errors"
	"fmt"
	"net/url"
)

func (a *App) connect(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: kioku connect google|github")
	}
	var v struct {
		URL string `json:"url"`
	}
	if _, err := a.request("POST", "/api/connect/"+url.PathEscape(args[0]), nil, &v); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, v.URL)
	openBrowser(v.URL)
	return nil
}
