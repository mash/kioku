package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func setupLoopback(t *testing.T) *fixture {
	t.Helper()
	f := setupWithWorker(t, false)
	f.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	f.s.Config.Providers["google"] = auth.Provider{Name: "google", ClientID: "id", ClientSecret: "secret", RedirectURL: f.url + "/auth/google/callback", Client: &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		var payload any
		switch r.URL.Host {
		case "oauth2.googleapis.com":
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("code") == "exchange-error" {
				return nil, errors.New("exchange failed")
			}
			payload = map[string]any{"access_token": r.Form.Get("code"), "expires_in": 3600}
		case "openidconnect.googleapis.com":
			id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if id == "identity-error" {
				return nil, errors.New("identity failed")
			}
			payload = map[string]any{"sub": id, "name": id, "email": id + "@example.com", "email_verified": true}
		default:
			return nil, fmt.Errorf("unexpected host %s", r.URL.Host)
		}
		b, _ := json.Marshal(payload)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}, nil
	})}}
	return f
}

type loopbackAttempt struct {
	device, verifier, state, redirect, startURL string
}

func newLoopbackAttempt(t *testing.T, f *fixture) loopbackAttempt {
	t.Helper()
	a := loopbackAttempt{verifier: auth.Token(), state: auth.Token(), redirect: "http://127.0.0.1:5000/callback"}
	var result map[string]string
	if status := f.request("POST", "/api/auth/loopback", "", map[string]string{"provider": "google", "code_challenge": auth.Challenge(a.verifier), "code_challenge_method": "S256", "redirect_uri": a.redirect, "state": a.state}, &result); status != 201 {
		t.Fatalf("start: %d %v", status, result)
	}
	if len(result) != 2 || result["secret"] != "" {
		t.Fatalf("unexpected start response: %v", result)
	}
	a.device, a.startURL = result["device"], result["url"]
	return a
}

func startLoopbackOAuth(t *testing.T, f *fixture, a loopbackAttempt) *http.Request {
	t.Helper()
	resp, err := f.client.Get(a.startURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Fatalf("OAuth start: %d", resp.StatusCode)
	}
	u, err := resp.Location()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("GET", f.url+"/auth/google/callback?"+url.Values{"state": {u.Query().Get("state")}, "code": {"victim"}}.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	return req
}

func finishLoopbackOAuth(t *testing.T, f *fixture, req *http.Request) *http.Response {
	t.Helper()
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func loopbackSnapshot(t *testing.T, f *fixture) model.State {
	t.Helper()
	var state model.State
	if err := f.db.View(func(st model.State) error { state = st; return nil }); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestLoopbackStartValidation(t *testing.T) {
	for _, tc := range []struct{ field, value string }{
		{"provider", "unknown"},
		{"redirect_uri", "http://localhost:5000/callback"},
		{"redirect_uri", "http://[::1]:5000/callback"},
		{"redirect_uri", "https://127.0.0.1:5000/callback"},
		{"redirect_uri", "http://example.com:5000/callback"},
		{"redirect_uri", "http://127.0.0.1.evil.example:5000/callback"},
		{"redirect_uri", "http://127.0.0.1/callback"},
		{"redirect_uri", "http://127.0.0.1:0/callback"},
		{"redirect_uri", "http://127.0.0.1:65536/callback"},
		{"redirect_uri", "http://user@127.0.0.1:5000/callback"},
		{"redirect_uri", "http://127.0.0.1:5000/callback?x=1"},
		{"redirect_uri", "http://127.0.0.1:5000/callback?"},
		{"redirect_uri", "http://127.0.0.1:5000/callback#fragment"},
		{"redirect_uri", "http://127.0.0.1:5000/callback#"},
		{"redirect_uri", "http://127.0.0.1:5000/other"},
		{"redirect_uri", "http://127.0.0.1:5000/%63allback"},
		{"code_challenge_method", "plain"},
		{"code_challenge", "short"},
		{"code_challenge", strings.Repeat("+", 43)},
		{"code_challenge", auth.Challenge("v") + "="},
		{"state", ""},
		{"state", strings.Repeat("a", 129)},
		{"state", "a b"},
		{"state", "日本語"},
	} {
		t.Run(tc.field+"/"+tc.value, func(t *testing.T) {
			f := setupLoopback(t)
			req := map[string]string{"provider": "google", "code_challenge": auth.Challenge(auth.Token()), "code_challenge_method": "S256", "redirect_uri": "http://127.0.0.1:5000/callback", "state": "abc-._~"}
			req[tc.field] = tc.value
			if status := f.request("POST", "/api/auth/loopback", "", req, nil); status != 400 {
				t.Fatal(status)
			}
			if len(loopbackSnapshot(t, f).Devices) != 0 {
				t.Fatal("invalid request created a device")
			}
		})
	}
}

func TestLoopbackExchangeConsumesFailures(t *testing.T) {
	for _, failure := range []string{"missing code", "wrong code", "wrong verifier", "short verifier", "long verifier", "invalid verifier", "expired", "unissued", "non-loopback"} {
		t.Run(failure, func(t *testing.T) {
			f := setupLoopback(t)
			a := newLoopbackAttempt(t, f)
			code := ""
			if failure != "unissued" {
				resp := finishLoopbackOAuth(t, f, startLoopbackOAuth(t, f, a))
				if resp.StatusCode != 302 {
					t.Fatal(resp.StatusCode)
				}
				u, err := resp.Location()
				if err != nil {
					t.Fatal(err)
				}
				code = u.Query().Get("code")
			}
			req := map[string]string{"device": a.device, "code": code, "code_verifier": a.verifier}
			switch failure {
			case "missing code":
				delete(req, "code")
			case "wrong code":
				req["code"] = "kioku_loopback.wrong"
			case "wrong verifier":
				req["code_verifier"] = auth.Token()
			case "short verifier":
				req["code_verifier"] = "short"
			case "long verifier":
				req["code_verifier"] = strings.Repeat("a", 129)
			case "invalid verifier":
				req["code_verifier"] = strings.Repeat("+", 43)
			case "expired", "non-loopback":
				if err := f.db.Update(func(st *model.State) error {
					d := st.Devices[a.device]
					if failure == "expired" {
						d.Expires = time.Now().Add(-time.Second)
					} else {
						d.LoopbackChallenge = ""
					}
					st.Devices[a.device] = d
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := loopbackSnapshot(t, f)
			if status := f.request("POST", "/api/auth/loopback/token", "", req, nil); status != 410 {
				t.Fatal(status)
			}
			after := loopbackSnapshot(t, f)
			if !after.Devices[a.device].Used || !reflect.DeepEqual(before.Sessions, after.Sessions) {
				t.Fatal("failed exchange was not consumed or issued a session")
			}
			req["code"], req["code_verifier"] = code, a.verifier
			if status := f.request("POST", "/api/auth/loopback/token", "", req, nil); status != 410 {
				t.Fatalf("retry: %d", status)
			}
		})
	}
}

func TestLoopbackCallbackFailure(t *testing.T) {
	for _, reason := range []string{"denied", "exchange-error", "identity-error"} {
		for _, unusable := range []string{"", "expired", "used", "bound", "missing"} {
			t.Run(reason+"/"+unusable, func(t *testing.T) {
				f := setupLoopback(t)
				a := newLoopbackAttempt(t, f)
				req := startLoopbackOAuth(t, f, a)
				q := req.URL.Query()
				want := "server_error"
				if reason == "denied" {
					q.Set("error", "access_denied")
					// A denial must bypass exchange even when its code would fail.
					q.Set("code", "exchange-error")
					want = "access_denied"
				} else {
					q.Set("code", reason)
				}
				req.URL.RawQuery = q.Encode()
				if err := f.db.Update(func(st *model.State) error {
					d := st.Devices[a.device]
					switch unusable {
					case "expired":
						d.Expires = time.Now().Add(-time.Second)
					case "used":
						d.Used = true
					case "bound":
						d.UserID = "other"
					case "missing":
						delete(st.Devices, a.device)
						return nil
					}
					st.Devices[a.device] = d
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				before := loopbackSnapshot(t, f)
				resp := finishLoopbackOAuth(t, f, req)
				if unusable == "" {
					u, err := resp.Location()
					if err != nil || resp.StatusCode != 302 || u.Scheme+"://"+u.Host+u.Path != a.redirect || u.Query().Get("error") != want || u.Query().Get("state") != a.state || u.Query().Get("code") != "" {
						t.Fatalf("failure redirect: %d %v %v", resp.StatusCode, u, err)
					}
				} else if resp.StatusCode != 410 || resp.Header.Get("Location") != "" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
					t.Fatalf("stale failure callback: %d %v", resp.StatusCode, resp.Header)
				}
				after := loopbackSnapshot(t, f)
				if !reflect.DeepEqual(before.Devices, after.Devices) || !reflect.DeepEqual(before.Users, after.Users) || !reflect.DeepEqual(before.Sessions, after.Sessions) {
					t.Fatal("failure changed login state")
				}
				if resp := finishLoopbackOAuth(t, f, req); resp.StatusCode != 403 {
					t.Fatal("OAuth state reused", resp.StatusCode)
				}
			})
		}
	}
}

func TestLoopbackDuplicateCallbackAndRevocation(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprint(revoke), func(t *testing.T) {
			f := setupLoopback(t)
			a := newLoopbackAttempt(t, f)
			first, second := startLoopbackOAuth(t, f, a), startLoopbackOAuth(t, f, a)
			resp := finishLoopbackOAuth(t, f, first)
			u, err := resp.Location()
			if err != nil || resp.StatusCode != 302 {
				t.Fatalf("callback: %d %v", resp.StatusCode, err)
			}
			before := loopbackSnapshot(t, f)
			d := before.Devices[a.device]
			if d.LoopbackCodeHash != auth.Hash(u.Query().Get("code")) || d.SecretHash != "" || d.UserID == "" || d.IdentityCredential == "" || time.Until(d.Expires) <= 0 || time.Until(d.Expires) > time.Minute {
				t.Fatal("invalid issued device")
			}
			q := second.URL.Query()
			q.Set("code", "another-user")
			second.URL.RawQuery = q.Encode()
			if resp := finishLoopbackOAuth(t, f, second); resp.StatusCode != 410 {
				t.Fatal(resp.StatusCode)
			}
			after := loopbackSnapshot(t, f)
			if !reflect.DeepEqual(before.Devices, after.Devices) || !reflect.DeepEqual(before.Users, after.Users) || !reflect.DeepEqual(before.Sessions, after.Sessions) {
				t.Fatal("duplicate callback changed account or device")
			}
			want := 200
			if revoke {
				var token string
				for _, c := range resp.Cookies() {
					if c.Name == "kioku_session" {
						token = c.Value
					}
				}
				if status := f.request("POST", "/api/account/revoke-all", token, nil, nil); status != 200 {
					t.Fatal(status)
				}
				want = 410
			}
			if status := f.request("POST", "/api/auth/loopback/token", "", map[string]string{"device": a.device, "code": u.Query().Get("code"), "code_verifier": a.verifier}, nil); status != want {
				t.Fatal(status)
			}
		})
	}
}

func TestLoopbackAndConnectSeparation(t *testing.T) {
	f := setupLoopback(t)
	a := newLoopbackAttempt(t, f)
	if status := f.request("GET", "/auth/google/start?connect="+a.device+"&ticket=anything", "", nil, nil); status != 403 {
		t.Fatal(status)
	}
	token := f.login("owner@example.com")
	var connection map[string]string
	if status := f.request("POST", "/api/connect/google", token, nil, &connection); status != 201 {
		t.Fatal(status)
	}
	u, err := url.Parse(connection["url"])
	if err != nil {
		t.Fatal(err)
	}
	if status := f.request("GET", "/auth/google/start?device="+u.Query().Get("connect"), "", nil, nil); status != 410 {
		t.Fatal(status)
	}
	resp, err := f.client.Get(connection["url"])
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Fatal("connect broken", resp.StatusCode)
	}
	providerURL, err := resp.Location()
	if err != nil {
		t.Fatal(err)
	}
	st := loopbackSnapshot(t, f)
	uid := st.Sessions[auth.Hash(token)].UserID
	callback, err := http.NewRequest("GET", f.url+"/auth/google/callback?"+url.Values{"state": {providerURL.Query().Get("state")}, "code": {uid}}.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range resp.Cookies() {
		callback.AddCookie(cookie)
	}
	connected := finishLoopbackOAuth(t, f, callback)
	if connected.StatusCode != 303 || connected.Header.Get("Location") != "/" || loopbackSnapshot(t, f).Users[uid].Connections["google"] == "" {
		t.Fatal("connect callback broken", connected.StatusCode)
	}
	if err := f.db.Update(func(st *model.State) error {
		d := st.Devices[a.device]
		d.Expires = time.Now().Add(-time.Second)
		st.Devices[a.device] = d
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if status := f.request("GET", "/auth/google/start?device="+a.device, "", nil, nil); status != 410 {
		t.Fatal(status)
	}
	for _, path := range []string{"/api/auth/device", "/api/auth/device/poll"} {
		if status := f.request("POST", path, "", nil, nil); status != 405 {
			t.Fatal("old endpoint remains", status)
		}
	}
}

func TestLoopbackLimitsAndPruning(t *testing.T) {
	f := setupLoopback(t)
	a := newLoopbackAttempt(t, f)
	if err := f.db.Update(func(st *model.State) error {
		d := st.Devices[a.device]
		for i := range 10000 {
			st.Devices[strconv.Itoa(i)] = d
		}
		delete(st.Devices, a.device)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := map[string]string{"provider": "google", "code_challenge": auth.Challenge(a.verifier), "code_challenge_method": "S256", "redirect_uri": a.redirect, "state": a.state}
	if status := f.request("POST", "/api/auth/loopback", "", req, nil); status != 429 {
		t.Fatal(status)
	}
	if err := f.db.Update(func(st *model.State) error { d := st.Devices["0"]; d.Used = true; st.Devices["0"] = d; return nil }); err != nil {
		t.Fatal(err)
	}
	if status := f.request("POST", "/api/auth/loopback", "", req, nil); status != 201 {
		t.Fatal(status)
	}
	if len(loopbackSnapshot(t, f).Devices) != 10000 {
		t.Fatal("capacity not enforced")
	}
	s := &Server{Config: Config{RatePerMinute: 100}}
	for i := range 21 {
		paths := []string{"/auth/google/start", "/api/auth/loopback", "/api/auth/loopback/token"}
		r := httptest.NewRequest("POST", paths[i%len(paths)], nil)
		r.RemoteAddr = "127.0.0.1:5000"
		if s.allow(r) != (i < 20) {
			t.Fatal("auth rate limit", i)
		}
	}
}

func TestLoopbackTokenInvalidJSON(t *testing.T) {
	f := setupLoopback(t)
	for _, body := range []string{"{", "null", "[]", `{"device":42}`, `{"unknown":true}`, `{} {}`} {
		req, err := http.NewRequest("POST", f.url+"/api/auth/loopback/token", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 || !strings.Contains(string(data), "invalid request") {
			t.Fatalf("%s: %d %s", body, resp.StatusCode, data)
		}
	}
	if status := f.request("POST", "/api/auth/loopback/token", "", map[string]string{"device": "missing"}, nil); status != 410 {
		t.Fatal(status)
	}
}

func TestLoopbackConcurrentExchange(t *testing.T) {
	f := setupLoopback(t)
	a := newLoopbackAttempt(t, f)
	callback := finishLoopbackOAuth(t, f, startLoopbackOAuth(t, f, a))
	u, err := callback.Location()
	if err != nil {
		t.Fatal(err)
	}
	before := loopbackSnapshot(t, f)
	body, err := json.Marshal(map[string]string{"device": a.device, "code": u.Query().Get("code"), "code_verifier": a.verifier})
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(chan int, 2)
	for range 2 {
		go func() {
			resp, err := f.client.Post(f.url+"/api/auth/loopback/token", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Error(err)
				statuses <- 0
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	first, second := <-statuses, <-statuses
	if !(first == 200 && second == 410 || first == 410 && second == 200) {
		t.Fatal(first, second)
	}
	if len(loopbackSnapshot(t, f).Sessions) != len(before.Sessions)+1 {
		t.Fatal("concurrent exchange issued extra sessions")
	}
}

func TestLoopbackOAuthCookieMismatch(t *testing.T) {
	f := setupLoopback(t)
	a := newLoopbackAttempt(t, f)
	req := startLoopbackOAuth(t, f, a)
	req.Header.Del("Cookie")
	before := loopbackSnapshot(t, f)
	if resp := finishLoopbackOAuth(t, f, req); resp.StatusCode != 403 {
		t.Fatal(resp.StatusCode)
	}
	after := loopbackSnapshot(t, f)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("state mismatch changed state")
	}
}
