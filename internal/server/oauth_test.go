package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mash/kioku/internal/auth"
)

type oauthTransport func(*http.Request) (*http.Response, error)

func (f oauthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestOAuthDeviceFlowPKCEAndAccountIsolation(t *testing.T) {
	f := setup(t)
	f.s.Config.Providers["google"] = auth.Provider{Name: "google", ClientID: "id", ClientSecret: "secret", RedirectURL: f.url + "/auth/google/callback"}
	old := auth.Client
	defer func() { auth.Client = old }()
	challenge := ""
	providerEmail := "same@example.com"
	auth.Client = &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
		var payload any
		switch r.URL.Host {
		case "oauth2.googleapis.com":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if auth.Challenge(r.Form.Get("code_verifier")) != challenge {
				t.Error("PKCE mismatch")
			}
			payload = map[string]any{"access_token": r.Form.Get("code"), "refresh_token": "refresh-secret", "expires_in": 3600}
		case "openidconnect.googleapis.com":
			payload = map[string]any{"sub": strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "name": "Same email", "email": providerEmail, "email_verified": true}
		default:
			t.Errorf("unexpected host %s", r.URL.Host)
		}
		b, _ := json.Marshal(payload)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})}
	login := func(identity string) string {
		t.Helper()
		var d map[string]string
		if code := f.request("POST", "/api/auth/device", "", map[string]string{"provider": "google"}, &d); code != 201 {
			t.Fatal(code)
		}
		var pending map[string]string
		if code := f.request("POST", "/api/auth/device/poll", "", map[string]string{"device": d["device"], "secret": d["secret"]}, &pending); code != 202 {
			t.Fatal(code)
		}
		client := *f.client
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Get(d["url"])
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 302 {
			t.Fatal(resp.StatusCode)
		}
		u, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		challenge = u.Query().Get("code_challenge")
		if challenge == "" {
			t.Fatal("PKCE missing")
		}
		state := u.Query().Get("state")
		cookies := resp.Cookies()
		if len(cookies) == 0 {
			t.Fatal("OAuth cookie missing")
		}
		req, _ := http.NewRequest("GET", f.url+"/auth/google/callback?state="+url.QueryEscape(state)+"&code="+identity, nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("callback: %d", resp.StatusCode)
		}
		var result map[string]string
		if code := f.request("POST", "/api/auth/device/poll", "", map[string]string{"device": d["device"], "secret": d["secret"]}, &result); code != 200 || result["token"] == "" {
			t.Fatalf("poll %d %v", code, result)
		}
		if code := f.request("POST", "/api/auth/device/poll", "", map[string]string{"device": d["device"], "secret": d["secret"]}, nil); code != 410 {
			t.Fatal("device was reusable")
		}
		return result["token"]
	}
	first, second := login("google-user-one"), login("google-user-two")
	var a, b struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	f.request("GET", "/api/me", first, nil, &a)
	f.request("GET", "/api/me", second, nil, &b)
	if a.ID == b.ID || a.Email != b.Email {
		t.Fatal("accounts merged by email")
	}
	providerEmail = "new@example.com"
	third := login("google-user-one")
	var refreshed struct{ ID, Email string }
	f.request("GET", "/api/me", third, nil, &refreshed)
	if refreshed.ID != a.ID || refreshed.Email != "new@example.com" {
		t.Fatal("repeat login did not refresh the verified profile")
	}

}
