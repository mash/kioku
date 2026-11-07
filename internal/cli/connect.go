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
	if _, err := browserOrigin(a.Config.Server, v.URL); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, v.URL)
	if err := openBrowser(a.Config.Server, v.URL); err != nil {
		fmt.Fprintf(a.Err, "ブラウザを開けませんでした。表示された URL をブラウザに貼り付けてください: %v\n", err)
	}
	return nil
}
