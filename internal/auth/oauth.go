package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

type Provider struct {
	Name, ClientID, ClientSecret, RedirectURL string
	Client                                    *http.Client
}
type Credential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}
type Identity struct{ ID, Name, Email string }

var Client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// oauthContext preserves the provider timeout and redirect policy for token and
// JWKS requests, including the injected transport used by tests.
func (p Provider) oauthContext(ctx context.Context) context.Context {
	client := p.Client
	if client == nil {
		client = Client
	}
	return context.WithValue(ctx, oauth2.HTTPClient, client)
}

func (p Provider) config(purpose string) oauth2.Config {
	endpoint, scopes := googleConfig(purpose)
	if p.Name == "github" {
		endpoint, scopes = githubConfig(purpose)
	}

	// Both providers accept credentials in the POST body; avoid auth-style probing.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return oauth2.Config{ClientID: p.ClientID, ClientSecret: p.ClientSecret, RedirectURL: p.RedirectURL, Endpoint: endpoint, Scopes: scopes}
}

func (p Provider) URL(state, verifier, purpose string) string {
	config := p.config(purpose)
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}
	if p.Name == "google" {
		opts = append(opts, googleAuthOptions(verifier, purpose)...)
	}
	return config.AuthCodeURL(state, opts...)
}

func credential(token *oauth2.Token) Credential {
	return Credential{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, ExpiresAt: token.Expiry}
}

// Exchange verifies the login identity before returning credentials to the caller.
// ID tokens are transient and are never persisted in a session or connection.
func (p Provider) Exchange(ctx context.Context, code, verifier string) (Credential, Identity, error) {
	ctx = p.oauthContext(ctx)
	config := p.config("login")
	token, err := config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Credential{}, Identity{}, err
	}
	var identity Identity
	if p.Name == "google" {
		identity, err = p.googleLoginIdentity(ctx, token, verifier)
	} else {
		identity, err = p.githubIdentity(ctx, token.AccessToken)
	}
	if err != nil {
		return Credential{}, Identity{}, err
	}

	return credential(token), identity, nil
}

func (p Provider) Refresh(ctx context.Context, c Credential) (Credential, error) {
	if c.ExpiresAt.IsZero() || time.Until(c.ExpiresAt) > time.Minute {
		return c, nil
	}
	if c.RefreshToken == "" {
		return c, errors.New("connection expired; reconnect the provider")
	}
	config := p.config("connect")
	token := &oauth2.Token{AccessToken: c.AccessToken, RefreshToken: c.RefreshToken, Expiry: c.ExpiresAt}
	source := oauth2.ReuseTokenSourceWithExpiry(token, config.TokenSource(p.oauthContext(ctx), token), time.Minute)
	fresh, err := source.Token()
	if err != nil {
		return Credential{}, err
	}
	return credential(fresh), nil
}
func (p Provider) Identity(ctx context.Context, token string) (Identity, error) {
	if p.Name == "google" {
		return p.googleIdentity(ctx, token)
	}
	return p.githubIdentity(ctx, token)
}
func Get(ctx context.Context, u, token string, out any) error {
	return (Provider{}).get(ctx, u, token, out)
}
func (p Provider) get(ctx context.Context, u, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kioku-wiki")
	return p.doJSON(req, out)
}
func (p Provider) doJSON(req *http.Request, out any) error {
	client := p.Client
	if client == nil {
		client = Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}
