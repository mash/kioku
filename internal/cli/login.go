package cli

import (
	"errors"
	"flag"
	"fmt"
	"time"
)

func (a *App) login(args []string) error {
	f := flag.NewFlagSet("login", flag.ContinueOnError)
	f.SetOutput(a.Err)
	provider := f.String("provider", "google", "google or github")
	if err := f.Parse(args); err != nil {
		return err
	}
	var device struct{ Device, Secret, URL string }
	if _, err := a.request("POST", "/api/auth/device", map[string]string{"provider": *provider}, &device); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "ブラウザで認証してください:\n%s\n", device.URL)
	openBrowser(device.URL)
	timer := time.NewTimer(10 * time.Minute)
	defer timer.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()
		case <-timer.C:
			return errors.New("login timed out")
		case <-ticker.C:
			var v struct {
				Token  string `json:"token"`
				Status string `json:"status"`
			}
			status, err := a.request("POST", "/api/auth/device/poll", map[string]string{"device": device.Device, "secret": device.Secret}, &v)
			if err != nil {
				return err
			}
			if status == 200 {
				a.Config.Token = v.Token
				if err = a.saveCredentials(); err != nil {
					return err
				}
				fmt.Fprintln(a.Out, "ログインしました。")
				return nil
			}
		}
	}
}
