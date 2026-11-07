package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestConnectLoopbackPreservesLogin(t *testing.T) {
	f := newAuthFixture(t, true)
	before, err := os.ReadFile(f.app.credentialPath())
	if err != nil {
		t.Fatal(err)
	}
	var callback string
	stubBrowser(t, func(server, target string) error {
		start := <-f.started
		if server != f.server.URL || !strings.Contains(f.out.String(), "接続先: "+server) || !strings.Contains(f.out.String(), target) {
			t.Fatal("connection target not displayed")
		}
		checkLoginPage(t, fetchLoginPage(start["redirect_uri"]+"?state=wrong&code="+loopbackTestCode), 400, "要求と一致しません")
		if f.exchangeCount() != 0 {
			t.Fatal("mismatched state completed connection")
		}
		callback = loginCallbackURL(start, url.Values{"code": {loopbackTestCode}})
		checkLoginPage(t, fetchLoginPage(callback), 200, "接続しました: google")
		checkLoginPage(t, fetchLoginPage(callback), 200, "この接続は終わりました")
		return nil
	})
	if err := f.app.connect([]string{"google"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(f.app.credentialPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || f.app.Config.Token != "existing-human-token" || f.exchangeCount() != 1 {
		t.Fatal("connect changed login or exchanged twice")
	}
	if response := fetchLoginPage(callback); response.err == nil {
		t.Fatal("listener remained open")
	}
}

func TestConnectLoopbackFailures(t *testing.T) {
	for _, mode := range []string{"cancelled", "missing code", "server rejected", "bad response", "cancel context"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuthFixture(t, true)
			before, err := os.ReadFile(f.app.credentialPath())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(f.app.ctx)
			defer cancel()
			f.app.ctx = ctx
			if mode == "server rejected" {
				f.token = func(w http.ResponseWriter) {
					w.WriteHeader(410)
					json.NewEncoder(w).Encode(map[string]string{"error": "expired connection"})
				}
			}
			if mode == "bad response" {
				f.token = func(w http.ResponseWriter) {
					json.NewEncoder(w).Encode(map[string]string{"token": "unexpected-login-token"})
				}
			}
			var callback string
			stubBrowser(t, func(_, _ string) error {
				start := <-f.started
				q := url.Values{"code": {loopbackTestCode}}
				if mode == "cancelled" {
					q = url.Values{"error": {"access_denied"}}
				}
				if mode == "missing code" {
					q = url.Values{}
				}
				callback = loginCallbackURL(start, q)
				if mode == "cancel context" {
					cancel()
					return nil
				}
				checkLoginPage(t, fetchLoginPage(callback), 400, "接続に失敗しました")
				return nil
			})
			if err := f.app.connect([]string{"google"}); err == nil {
				t.Fatal("failed connection reported success")
			}
			after, err := os.ReadFile(f.app.credentialPath())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || f.app.Config.Token != "existing-human-token" {
				t.Fatal("failed connection changed login")
			}
			if response := fetchLoginPage(callback); response.err == nil {
				t.Fatal("listener leaked")
			}
		})
	}
}

func TestGitHubAppLoopbackAndRestartGuidance(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			f := newAuthProviderFixture(t, true, "github")
			before, err := os.ReadFile(f.app.credentialPath())
			if err != nil {
				t.Fatal(err)
			}
			stubBrowser(t, func(_, _ string) error {
				start := <-f.started
				q := url.Values{"code": {loopbackTestCode}}
				want := 200
				if restart {
					q = url.Values{"error": {"restart_required"}}
					want = 400
				}
				response := fetchLoginPage(loginCallbackURL(start, q))
				if response.status != want {
					t.Fatal(response.status, response.body)
				}
				return nil
			})
			err = f.app.connect([]string{"github"})
			if (err != nil) != restart {
				t.Fatal(err)
			}
			if restart && !strings.Contains(err.Error(), "GitHub App") {
				t.Fatal("missing migration instructions", err)
			}
			if !restart && (!strings.Contains(f.out.String(), "installation") || !strings.Contains(f.out.String(), "kioku put URL")) {
				t.Fatal(f.out.String())
			}
			after, _ := os.ReadFile(f.app.credentialPath())
			if !bytes.Equal(before, after) {
				t.Fatal("App changed CLI login")
			}
		})
	}
}
func TestGitHubAppAccountStatusKeepsStructuredOutput(t *testing.T) {
	out, diagnostics := new(bytes.Buffer), new(bytes.Buffer)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"connections": []string{}, "github_app": map[string]any{"configured": false, "enabled": false, "status": "not_connected", "installations": []any{}}})
	}))
	defer server.Close()
	a := &App{Config: Config{Server: server.URL, Token: "human"}, Client: server.Client(), Out: out, Err: diagnostics, ctx: context.Background()}
	if err := a.accountStatus([]string{"status"}); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if json.Unmarshal(out.Bytes(), &v) != nil || v["github_app"] == nil || !strings.Contains(diagnostics.String(), "運用者の設定") {
		t.Fatal(out.String(), diagnostics.String())
	}
}
