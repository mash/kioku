package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mash/kioku/internal/authtest"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonResponse(t *testing.T, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b)))}
}

func TestGoogleExchangeValidatesOIDC(t *testing.T) {
	google := authtest.NewGoogle(t)
	other := authtest.NewGoogle(t)
	verifier := Token()
	for _, name := range []string{"valid", "missing", "signature", "issuer", "audience", "expired", "nonce", "missing-nonce", "empty-subject", "userinfo-subject", "unverified-email"} {
		t.Run(name, func(t *testing.T) {
			claims := google.Claims("user", verifier)
			switch name {
			case "issuer":
				claims["iss"] = "https://attacker.example"
			case "audience":
				claims["aud"] = "another-client"
			case "expired":
				claims["exp"] = time.Now().Add(-time.Minute).Unix()
			case "nonce":
				claims["nonce"] = "another-login"
			case "missing-nonce":
				delete(claims, "nonce")
			case "empty-subject":
				claims["sub"] = ""
			}
			raw := google.Sign(t, claims)
			if name == "signature" {
				raw = other.Sign(t, claims)
			}
			if name == "missing" {
				raw = ""
			}
			p := Provider{Name: "google", ClientID: "id", ClientSecret: "secret", RedirectURL: "https://kioku.example/auth/google/callback"}
			p.Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Host {
				case "oauth2.googleapis.com":
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if r.Form.Get("code_verifier") != verifier || r.Form.Get("client_id") != "id" || r.Form.Get("client_secret") != "secret" || r.Form.Get("redirect_uri") != p.RedirectURL {
						t.Fatal("incorrect token exchange parameters")
					}
					return jsonResponse(t, map[string]any{"access_token": "access", "refresh_token": "refresh", "expires_in": 3600, "id_token": raw}), nil
				case "www.googleapis.com":
					return jsonResponse(t, google.JWKS()), nil
				case "openidconnect.googleapis.com":
					sub := "user"
					if name == "userinfo-subject" {
						sub = "other"
					}
					return jsonResponse(t, map[string]any{"sub": sub, "email": "USER@example.com", "email_verified": name != "unverified-email"}), nil
				default:
					t.Fatalf("unexpected request %s", r.URL)
				}
				return nil, nil
			})}
			c, identity, err := p.Exchange(context.Background(), "code", verifier)
			if name == "valid" {
				if err != nil || identity.ID != "user" || identity.Email != "user@example.com" || c.RefreshToken != "refresh" {
					t.Fatalf("valid exchange: %v %+v %+v", err, c, identity)
				}
				b, _ := json.Marshal(c)
				if strings.Contains(string(b), raw) {
					t.Fatal("persisted ID token")
				}
			} else if err == nil || c.AccessToken != "" || identity.ID != "" {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

func TestProviderAuthorizationOptions(t *testing.T) {
	verifier := Token()
	for _, name := range []string{"google", "github"} {
		for _, purpose := range []string{"login", "connect"} {
			p := Provider{Name: name, ClientID: "id", RedirectURL: "https://kioku.example/callback"}
			u, err := url.Parse(p.URL("state", verifier, purpose))
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			if q.Get("state") != "state" || q.Get("code_challenge") != Challenge(verifier) || q.Get("code_challenge_method") != "S256" {
				t.Fatal("missing state or PKCE")
			}
			if name == "google" {
				if q.Get("nonce") != Challenge("kioku-oidc-nonce:"+verifier) || q.Get("nonce") == q.Get("code_challenge") {
					t.Fatal("invalid nonce")
				}
				if purpose == "connect" && (q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || !strings.Contains(q.Get("scope"), "drive.readonly")) {
					t.Fatal("lost Google connection options")
				}
			} else if q.Get("nonce") != "" || q.Has("scope") {
				t.Fatal("incorrect GitHub options")
			}
		}
	}
}

func TestRefreshPreservesOrRotatesToken(t *testing.T) {
	for _, replacement := range []string{"", "rotated"} {
		calls := 0
		p := Provider{Name: "google", ClientID: "id", ClientSecret: "secret", Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "original" {
				t.Fatal("incorrect refresh")
			}
			return jsonResponse(t, map[string]any{"access_token": "fresh", "refresh_token": replacement, "expires_in": 3600}), nil
		})}}
		c := Credential{AccessToken: "old", RefreshToken: "original", ExpiresAt: time.Now().Add(30 * time.Second)}
		fresh, err := p.Refresh(context.Background(), c)
		want := replacement
		if want == "" {
			want = "original"
		}
		if err != nil || fresh.AccessToken != "fresh" || fresh.RefreshToken != want || calls != 1 {
			t.Fatalf("refresh: %+v %v calls=%d", fresh, err, calls)
		}
		if _, err := p.Refresh(context.Background(), fresh); err != nil || calls != 1 {
			t.Fatal("unnecessary refresh")
		}
	}
}

func TestGitHubAppExchangeVerifiesIdentityAndEmail(t *testing.T) {
	for _, outcome := range []string{"valid", "unverified", "not-primary", "no-email", "denied", "invalid-user"} {
		t.Run(outcome, func(t *testing.T) {
			verifier := Token()
			p := Provider{Name: "github", ClientID: "Iv.fixture", ClientSecret: "app-secret", RedirectURL: "https://kioku.example/auth/github/callback"}
			p.Client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "github.com" && r.URL.Path == "/login/oauth/access_token" {
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if r.Method != "POST" || r.Form.Get("client_id") != p.ClientID || r.Form.Get("client_secret") != p.ClientSecret || r.Form.Get("redirect_uri") != p.RedirectURL || r.Form.Get("code_verifier") != verifier || r.Form.Get("code") != "code" || r.Form.Has("scope") {
						t.Fatal("incorrect App login exchange")
					}
					return jsonResponse(t, map[string]any{"access_token": "ghu_login", "refresh_token": "ghr_login", "expires_in": 28800}), nil
				}
				if r.Method != "GET" || r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer ghu_login" {
					t.Fatal("incorrect identity request")
				}
				switch r.URL.Path {
				case "/user":
					id := 123
					if outcome == "invalid-user" {
						id = 0
					}
					return jsonResponse(t, map[string]any{"id": id, "login": "user"}), nil
				case "/user/emails":
					if outcome == "denied" {
						resp := jsonResponse(t, map[string]string{"message": "forbidden"})
						resp.StatusCode = 403
						return resp, nil
					}
					emails := []map[string]any{{"email": "USER@example.com", "primary": outcome != "not-primary", "verified": outcome != "unverified"}}
					if outcome == "no-email" {
						emails = nil
					}
					return jsonResponse(t, emails), nil
				default:
					t.Fatalf("unexpected request %s", r.URL)
					return nil, nil
				}
			})}
			c, identity, err := p.Exchange(context.Background(), "code", verifier)
			if outcome != "valid" {
				if err == nil || identity.ID != "" || c.AccessToken != "" {
					t.Fatal("accepted invalid identity or email")
				}
				return
			}
			if err != nil || identity.ID != "123" || identity.Email != "user@example.com" || identity.Name != "user" || c.AccessToken != "ghu_login" || c.RefreshToken != "ghr_login" || c.ExpiresAt.IsZero() {
				t.Fatalf("GitHub App exchange failed: %v", err)
			}
		})
	}
}

func TestUnknownProviderDoesNotUseAnotherProvider(t *testing.T) {
	p := Provider{Name: "unknown", Client: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unknown provider made a request")
		return nil, nil
	})}}
	if p.URL("state", "verifier", "login") != "" {
		t.Fatal("unknown provider produced an authorization URL")
	}
	if _, _, err := p.Exchange(context.Background(), "code", "verifier"); err == nil {
		t.Fatal("unknown provider exchanged a code")
	}
	if _, err := p.Identity(context.Background(), "token"); err == nil {
		t.Fatal("unknown provider fetched an identity")
	}
	if _, err := p.Refresh(context.Background(), Credential{RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)}); err == nil {
		t.Fatal("unknown provider refreshed a token")
	}
}

func TestGitHubIdentityFindsPrimaryEmailOnLaterPage(t *testing.T) {
	pages := []string{}
	p := Provider{Name: "github", Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer login" {
			t.Fatalf("unexpected identity request: %s", r.URL)
		}
		switch r.URL.Path {
		case "/user":
			return jsonResponse(t, map[string]any{"id": 42, "login": "user"}), nil
		case "/user/emails":
			page := r.URL.Query().Get("page")
			pages = append(pages, page)
			if page == "2" {
				return jsonResponse(t, []map[string]any{{"email": "PRIMARY@example.com", "primary": true, "verified": true}}), nil
			}
			if len(pages) > 2 {
				t.Fatal("repeated email page")
			}
			emails := make([]map[string]any, 100)
			for i := range emails {
				emails[i] = map[string]any{"email": "secondary@example.com", "primary": false, "verified": true}
			}
			return jsonResponse(t, emails), nil
		default:
			t.Fatalf("unexpected identity request: %s", r.URL)
			return nil, nil
		}
	})}}
	i, err := p.Identity(context.Background(), "login")
	if err != nil || i.ID != "42" || i.Email != "primary@example.com" || strings.Join(pages, ",") != "1,2" {
		t.Fatalf("paginated primary email: %+v, pages=%v, error=%v", i, pages, err)
	}
}
