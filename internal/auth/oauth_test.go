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
		p := Provider{Name: name, ClientID: "id", RedirectURL: "https://kioku.example/callback"}
		u, err := url.Parse(p.URL("state", verifier))
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
			if q.Get("scope") != "openid email profile" || q.Get("access_type") != "" || q.Get("prompt") != "" {
				t.Fatal("incorrect Google login options", q)
			}
		} else if q.Get("nonce") != "" || q.Get("scope") != "read:user user:email" {
			t.Fatal("incorrect GitHub login options", q)
		}
	}
}

func TestRefreshPreservesOrRotatesToken(t *testing.T) {
	for _, name := range []string{"google", "github"} {
		for _, replacement := range []string{"", "rotated"} {
			calls := 0
			p := Provider{Name: name, ClientID: "id", ClientSecret: "secret", Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "original" || r.Form.Get("scope") != "" {
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
}

func TestGitHubExchangeWithoutIDToken(t *testing.T) {
	p := Provider{Name: "github", ClientID: "id", ClientSecret: "secret", Client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			return jsonResponse(t, map[string]any{"access_token": "github-token"}), nil
		case "/user":
			return jsonResponse(t, map[string]any{"id": 123, "login": "user"}), nil
		case "/user/emails":
			return jsonResponse(t, []map[string]any{{"email": "user@example.com", "primary": true, "verified": true}}), nil
		default:
			t.Fatalf("unexpected request %s", r.URL)
		}
		return nil, nil
	})}}
	c, identity, err := p.Exchange(context.Background(), "code", Token())
	if err != nil || identity.ID != "123" || c.AccessToken != "github-token" {
		t.Fatalf("GitHub exchange: %+v %v", identity, err)
	}
}

func TestUnknownProviderDoesNotUseAnotherProvider(t *testing.T) {
	p := Provider{Name: "unknown", Client: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unknown provider made a request")
		return nil, nil
	})}}
	if p.URL("state", "verifier") != "" {
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
