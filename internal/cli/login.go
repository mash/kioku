package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/mash/kioku/internal/auth"
)

func (a *App) login(args []string) error {
	f := flag.NewFlagSet("login", flag.ContinueOnError)
	f.SetOutput(a.Err)
	provider := f.String("provider", "google", "google or github")
	if err := f.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Minute)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	verifier, state := auth.Token(), auth.Token()
	// Keep the attempt's deadline and credential writes local to this login.
	attempt := *a
	attempt.ctx = ctx
	var device struct{ Device, URL string }
	if _, err := attempt.request("POST", "/api/auth/loopback", map[string]string{
		"provider": *provider, "code_challenge": auth.Challenge(verifier), "code_challenge_method": "S256",
		"redirect_uri": "http://" + listener.Addr().String() + "/callback", "state": state,
	}, &device); err != nil {
		return err
	}
	origin, err := browserOrigin(a.Config.Server, device.URL)
	if err != nil {
		return err
	}
	if device.Device == "" {
		return errors.New("server returned an empty login request; run kioku login again")
	}
	result := make(chan error, 1)
	var accountMessage string
	var mu sync.Mutex
	handled := false
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			loginPage(w, http.StatusMethodNotAllowed, "ログインを確認できませんでした", "認証を始めたブラウザに戻ってください。")
			return
		}
		q := r.URL.Query()
		if q.Get("state") != state {
			loginPage(w, http.StatusBadRequest, "ログインを確認できませんでした", "現在のログイン要求と一致しません。認証を始めたブラウザに戻ってください。")
			return
		}
		mu.Lock()
		alreadyHandled := handled
		handled = true
		mu.Unlock()
		if alreadyHandled {
			loginPage(w, http.StatusOK, "このログインは終わりました", "CLI に戻って結果を確認してください。")
			return
		}
		var err error
		var response struct{ Token, Provider, Name, Email string }
		switch {
		case q.Has("error"):
			if q.Get("error") == "access_denied" {
				err = errors.New("login was cancelled in the browser")
			} else {
				err = errors.New("login failed on the server; run kioku login again")
			}
		case q.Get("code") == "":
			err = errors.New("login callback did not include a code")
		default:
			_, err = attempt.request("POST", "/api/auth/loopback/token", map[string]string{"device": device.Device, "code": q.Get("code"), "code_verifier": verifier}, &response)
			if err == nil && response.Token == "" {
				err = errors.New("server returned an empty login token")
			}
			if err == nil {
				err = ctx.Err()
			}
			if err == nil {
				attempt.Config.Token = response.Token
				err = attempt.saveCredentials()
			}
		}
		if err != nil {
			loginPage(w, http.StatusBadRequest, "ログインに失敗しました", err.Error()+"\nkioku login をもう一度実行してください。")
		} else {
			accountMessage = fmt.Sprintf("ログインしました: %s (%s)", response.Email, response.Provider)
			loginPage(w, http.StatusOK, accountMessage, response.Name+"\nこのタブを閉じて CLI に戻ってください。")
		}
		result <- err
	})}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer func() {
		cancel()
		// Let the callback finish writing its page before closing the listener.
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	}()
	fmt.Fprintf(a.Out, "ログイン先: %s\n同じ端末のブラウザで認証してください:\n%s\n中断は Ctrl-C\n", origin, device.URL)
	if err := openBrowser(a.Config.Server, device.URL); err != nil {
		fmt.Fprintf(a.Err, "ブラウザを開けませんでした。表示された URL を同じ端末のブラウザに貼り付けてください: %v\n", err)
	}
	select {
	case err := <-result:
		if err != nil {
			return err
		}
		a.Config.Token = attempt.Config.Token
		fmt.Fprintln(a.Out, accountMessage)
		return nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("login timed out; run kioku login again")
		}
		return ctx.Err()
	case err := <-serveErr:
		return err
	}
}

func loginPage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html lang="ja"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s · kioku</title><h1>%s</h1><p>%s</p></html>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(message))
}
