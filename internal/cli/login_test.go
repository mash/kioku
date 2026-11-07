package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/authtest"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/server"
	"github.com/mash/kioku/internal/store"
)

const loopbackTestCode = "kioku_loopback.test-code"
const loopbackTestToken = "human-loopback-token"

type loginFixture struct {
	app         *App
	server      *httptest.Server
	out, errOut bytes.Buffer
	started     chan map[string]string
	exchanges   int
	token       func(http.ResponseWriter)
	mu          sync.Mutex
	start       map[string]string
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	f := &loginFixture{started: make(chan map[string]string, 1)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "" {
			t.Error("login sent a credential or wrong method", r.Method)
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		switch r.URL.Path {
		case "/api/auth/loopback":
			if req["provider"] != "google" || req["code_challenge_method"] != "S256" || len(req["code_challenge"]) != 43 || len(req["state"]) != 43 {
				t.Error("invalid login start", req)
			}
			u, err := url.Parse(req["redirect_uri"])
			if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "/callback" {
				t.Error("invalid listener URL", req["redirect_uri"])
			}
			f.mu.Lock()
			f.start = req
			f.mu.Unlock()
			f.started <- req
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"device": "test-device", "url": f.server.URL + "/auth/google/start?device=test-device"})
		case "/api/auth/loopback/token":
			f.mu.Lock()
			f.exchanges++
			challenge := f.start["code_challenge"]
			f.mu.Unlock()
			if req["device"] != "test-device" || req["code"] != loopbackTestCode || auth.Challenge(req["code_verifier"]) != challenge {
				t.Error("invalid exchange")
			}
			if f.token != nil {
				f.token(w)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"token": loopbackTestToken, "provider": "google", "name": "Test User", "email": "user@example.com"})
		default:
			t.Error("unexpected request", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	f.app = &App{AuthDir: t.TempDir(), Config: Config{Server: f.server.URL}, Client: f.server.Client(), Out: &f.out, Err: &f.errOut, ctx: ctx}
	f.app.Client.Timeout = 3 * time.Second
	return f
}

func stubBrowser(t *testing.T, fn func(string, string) error) {
	t.Helper()
	old := openBrowser
	openBrowser = fn
	t.Cleanup(func() { openBrowser = old })
}

func loginCallbackURL(start map[string]string, values url.Values) string {
	values.Set("state", start["state"])
	return start["redirect_uri"] + "?" + values.Encode()
}

type loginResponse struct {
	status int
	header http.Header
	body   string
	err    error
}

func fetchLoginPage(target string) loginResponse {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		return loginResponse{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return loginResponse{resp.StatusCode, resp.Header, string(body), err}
}

func checkLoginPage(t *testing.T, response loginResponse, status int, text string) {
	t.Helper()
	if response.err != nil || response.status != status || !strings.Contains(response.body, text) {
		t.Fatalf("page: %d %s %v", response.status, response.body, response.err)
	}
	if response.header.Get("Content-Type") != "text/html; charset=utf-8" || response.header.Get("Cache-Control") != "no-store" || response.header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("unsafe page headers", response.header)
	}
}

func checkSavedLogin(t *testing.T, f *loginFixture) {
	t.Helper()
	loaded := &App{AuthDir: f.app.AuthDir, Config: Config{Server: f.app.Config.Server}}
	if err := loaded.loadCredentials(); err != nil || loaded.Config.Token != loopbackTestToken {
		t.Fatal("credentials not saved", err)
	}
	info, err := os.Stat(loaded.credentialPath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe credential permissions", info, err)
	}
}

func TestLoginLoopbackSuccessAndStateMismatch(t *testing.T) {
	f := newLoginFixture(t)
	var callback string
	stubBrowser(t, func(server, target string) error {
		start := <-f.started
		if server != f.server.URL || !strings.Contains(f.out.String(), "ログイン先: "+server) || !strings.Contains(f.out.String(), target) || !strings.Contains(f.out.String(), "Ctrl-C") {
			t.Fatal("login target not displayed before launch")
		}
		wrong := start["redirect_uri"] + "?state=wrong&code=" + loopbackTestCode
		checkLoginPage(t, fetchLoginPage(wrong), 400, "ログイン要求と一致しません")
		if response := fetchLoginPage(strings.TrimSuffix(start["redirect_uri"], "/callback") + "/other"); response.err != nil || response.status != 404 {
			t.Fatal("unknown callback path accepted", response)
		}
		if f.exchangeCount() != 0 {
			t.Fatal("mismatched state exchanged a token")
		}
		post, err := http.Post(loginCallbackURL(start, url.Values{"code": {loopbackTestCode}}), "text/plain", nil)
		if err != nil {
			t.Fatal(err)
		}
		post.Body.Close()
		if post.StatusCode != 405 || post.Header.Get("Allow") != "GET" || post.Header.Get("Cache-Control") != "no-store" || post.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("unexpected callback method accepted", post.StatusCode)
		}
		callback = loginCallbackURL(start, url.Values{"code": {loopbackTestCode}})
		checkLoginPage(t, fetchLoginPage(callback), 200, "ログインしました: user@example.com (google)")
		checkSavedLogin(t, f)
		return nil
	})
	if err := f.app.login(nil); err != nil {
		t.Fatal(err)
	}
	if f.app.Config.Token != loopbackTestToken || f.exchangeCount() != 1 || !strings.Contains(f.out.String(), "ログインしました: user@example.com (google)\n") {
		t.Fatal("login did not complete")
	}
	if response := fetchLoginPage(callback); response.err == nil {
		t.Fatal("listener remained open")
	}
}

func TestLoginHandlesOnlyFirstMatchingCallback(t *testing.T) {
	f := newLoginFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.token = func(w http.ResponseWriter) {
		close(entered)
		<-release
		json.NewEncoder(w).Encode(map[string]string{"token": loopbackTestToken, "provider": "google", "name": "Test User", "email": "user@example.com"})
	}
	stubBrowser(t, func(_, _ string) error {
		start := <-f.started
		callback := loginCallbackURL(start, url.Values{"code": {loopbackTestCode}})
		first := make(chan loginResponse, 1)
		go func() { first <- fetchLoginPage(callback) }()
		select {
		case <-entered:
		case <-f.app.ctx.Done():
			t.Fatal("exchange did not start")
		}
		checkLoginPage(t, fetchLoginPage(callback), 200, "このログインは終わりました")
		once.Do(func() { close(release) })
		checkLoginPage(t, <-first, 200, "ログインしました")
		return nil
	})
	if err := f.app.login(nil); err != nil {
		t.Fatal(err)
	}
	if f.exchangeCount() != 1 {
		t.Fatal("callback was processed more than once")
	}
}

func TestLoginCallbackFailures(t *testing.T) {
	for _, failure := range []string{"access_denied", "server_error", "missing code", "exchange", "empty token", "save"} {
		t.Run(failure, func(t *testing.T) {
			f := newLoginFixture(t)
			values := url.Values{"code": {loopbackTestCode}}
			want := ""
			switch failure {
			case "access_denied", "server_error":
				values.Set("error", failure)
				want = "login failed on the server; run kioku login again"
				if failure == "access_denied" {
					want = "login was cancelled in the browser"
				}
			case "missing code":
				values.Del("code")
				want = "login callback did not include a code"
			case "exchange":
				f.token = func(w http.ResponseWriter) {
					w.WriteHeader(410)
					json.NewEncoder(w).Encode(map[string]string{"error": "expired <script>"})
				}
				want = "expired <script>"
			case "empty token":
				f.token = func(w http.ResponseWriter) { json.NewEncoder(w).Encode(map[string]string{"token": ""}) }
				want = "server returned an empty login token"
			case "save":
				f.app.AuthDir = filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(f.app.AuthDir, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			stubBrowser(t, func(_, _ string) error {
				start := <-f.started
				response := fetchLoginPage(loginCallbackURL(start, values))
				checkLoginPage(t, response, 400, "ログインに失敗しました")
				if !strings.Contains(response.body, "kioku login") || strings.Contains(response.body, "<script>") {
					t.Fatal("unsafe or incomplete failure page", response.body)
				}
				return nil
			})
			if err := f.app.login(nil); err == nil || want != "" && err.Error() != want {
				t.Fatalf("error: %v, want %q", err, want)
			}
			if f.app.Config.Token != "" || strings.Contains(f.out.String(), "ログインしました:") {
				t.Fatal("failed login reported success")
			}
			if failure != "save" {
				if _, err := os.Stat(f.app.credentialPath()); !os.IsNotExist(err) {
					t.Fatal("failed login saved credentials", err)
				}
			}
			if (failure == "access_denied" || failure == "server_error" || failure == "missing code") && f.exchangeCount() != 0 {
				t.Fatal("error callback exchanged code")
			}
		})
	}
}

func TestLoginWaitsAfterLauncherFailure(t *testing.T) {
	f := newLoginFixture(t)
	launched := make(chan struct{})
	stubBrowser(t, func(_, _ string) error { close(launched); return errors.New("launcher unavailable") })
	done := make(chan error, 1)
	go func() { done <- f.app.login(nil) }()
	start := <-f.started
	<-launched
	checkLoginPage(t, fetchLoginPage(loginCallbackURL(start, url.Values{"code": {loopbackTestCode}})), 200, "ログインしました")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.errOut.String(), "launcher unavailable") {
		t.Fatal("launcher failure not reported")
	}
	checkSavedLogin(t, f)
}

func TestLoginCancellationAndDeadlineCloseListener(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			f := newLoginFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			}
			defer cancel()
			f.app.ctx = ctx
			var callback string
			stubBrowser(t, func(_, _ string) error {
				start := <-f.started
				callback = start["redirect_uri"]
				if !deadline {
					cancel()
				}
				return nil
			})
			err := f.app.login(nil)
			if deadline {
				if err == nil || !strings.Contains(err.Error(), "login timed out") {
					t.Fatal(err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if callback == "" {
				t.Fatal("browser was never launched")
			}
			if response := fetchLoginPage(callback); response.err == nil {
				t.Fatal("listener remained open")
			}
			if f.exchangeCount() != 0 {
				t.Fatal("cancelled login exchanged a token")
			}
		})
	}
}

func (f *loginFixture) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges
}

// The attacker uses the victim's start URL in a separate browser, then relays
// the resulting loopback Location. Account visibility mitigates this accepted
// login-CSRF limitation; the exchange itself still succeeds.
func TestLoginReverseCSRFShowsAttackerAccount(t *testing.T) {
	google := authtest.NewGoogle(t)
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cipher, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	provider := auth.Provider{Name: "google", ClientID: "id", ClientSecret: "secret", Client: &http.Client{Transport: loginOAuthTransport(func(r *http.Request) (*http.Response, error) {
		var payload any
		switch r.URL.Host {
		case "oauth2.googleapis.com":
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("code") != "attacker-authorization-code" {
				t.Error("unexpected provider code")
			}
			payload = map[string]any{"access_token": "attacker-provider-token", "expires_in": 3600, "id_token": google.Token(t, "attacker-id", r.Form.Get("code_verifier"))}
		case "www.googleapis.com":
			payload = google.JWKS()
		case "openidconnect.googleapis.com":
			payload = map[string]any{"sub": "attacker-id", "name": "Attacker <script>", "email": "attacker@example.com", "email_verified": true}
		default:
			return nil, fmt.Errorf("unexpected provider request: %s", r.URL.Host)
		}
		body, _ := json.Marshal(payload)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}}
	s := server.New(server.Config{Providers: map[string]auth.Provider{"google": provider}}, db, local.Store{Dir: filepath.Join(dir, "objects")}, cipher)
	set, err := runners.Load(runners.Config{}, db, cipher, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetRunners(set)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	s.Config.BaseURL = ts.URL
	provider.RedirectURL = ts.URL + "/auth/google/callback"
	s.Config.Providers["google"] = provider
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	victim := &App{AuthDir: t.TempDir(), Config: Config{Server: ts.URL}, Client: ts.Client(), Out: &out, Err: io.Discard, ctx: ctx}
	stubBrowser(t, func(_, stolenStartURL string) error {
		attacker := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		start, err := attacker.Get(stolenStartURL)
		if err != nil {
			return err
		}
		start.Body.Close()
		if start.StatusCode != 302 {
			return fmt.Errorf("OAuth start: %d", start.StatusCode)
		}
		providerURL, err := start.Location()
		if err != nil {
			return err
		}
		callback, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/auth/google/callback?"+url.Values{"state": {providerURL.Query().Get("state")}, "code": {"attacker-authorization-code"}}.Encode(), nil)
		if err != nil {
			return err
		}
		for _, cookie := range start.Cookies() {
			callback.AddCookie(cookie)
		}
		response, err := attacker.Do(callback)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != 302 {
			return fmt.Errorf("OAuth callback: %d", response.StatusCode)
		}
		// The victim opens the attacker-supplied URL on the victim's machine.
		page := fetchLoginPage(response.Header.Get("Location"))
		checkLoginPage(t, page, 200, "ログインしました: attacker@example.com (google)")
		if !strings.Contains(page.body, "Attacker &lt;script&gt;") || strings.Contains(page.body, "<script>") {
			t.Fatal("account name was missing or not escaped", page.body)
		}
		return nil
	})
	if err := victim.login(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ログインしました: attacker@example.com (google)\n") {
		t.Fatal("reverse login CSRF silently succeeded", out.String())
	}
	loaded := &App{AuthDir: victim.AuthDir, Config: Config{Server: ts.URL}}
	if err := loaded.loadCredentials(); err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Token == "" {
		t.Fatal("missing saved session")
	}
	if err := db.View(func(st model.State) error {
		session := st.Sessions[auth.Hash(loaded.Config.Token)]
		if session.IdentityID != "attacker-id" || st.Users[session.UserID].Email != "attacker@example.com" {
			t.Error("scenario did not save the attacker's session")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type loginOAuthTransport func(*http.Request) (*http.Response, error)

func (f loginOAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
