package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mash/kioku/internal/connector"
	"github.com/mash/kioku/internal/connector/registry"
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

func (p Provider) config(purpose, verifier string) (oauth2.Config, []oauth2.AuthCodeOption, error) {
	implementation, ok := registry.ByName(p.Name)
	if !ok {
		return oauth2.Config{}, nil, fmt.Errorf("unsupported provider %q", p.Name)
	}
	settings := implementation.OAuth(purpose, verifier)

	return oauth2.Config{ClientID: p.ClientID, ClientSecret: p.ClientSecret, RedirectURL: p.RedirectURL, Endpoint: settings.Endpoint, Scopes: settings.Scopes}, settings.Options, nil
}

func (p Provider) URL(state, verifier, purpose string) string {
	config, options, err := p.config(purpose, verifier)
	if err != nil {
		return ""
	}
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}
	opts = append(opts, options...)
	return config.AuthCodeURL(state, opts...)
}

func credential(token *oauth2.Token) Credential {
	return Credential{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, ExpiresAt: token.Expiry}
}

// Exchange verifies the login identity before returning credentials to the caller.
// ID tokens are transient and are never persisted in a session or connection.
func (p Provider) Exchange(ctx context.Context, code, verifier string) (Credential, connector.Identity, error) {
	ctx = p.oauthContext(ctx)
	config, _, err := p.config("login", verifier)
	if err != nil {
		return Credential{}, connector.Identity{}, err
	}
	token, err := config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Credential{}, connector.Identity{}, err
	}
	implementation, _ := registry.ByName(p.Name)
	identity, err := implementation.LoginIdentity(ctx, p.get, p.ClientID, token, verifier)
	if err != nil {
		return Credential{}, connector.Identity{}, err
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
	config, _, err := p.config("connect", "")
	if err != nil {
		return Credential{}, err
	}
	token := &oauth2.Token{AccessToken: c.AccessToken, RefreshToken: c.RefreshToken, Expiry: c.ExpiresAt}
	source := oauth2.ReuseTokenSourceWithExpiry(token, config.TokenSource(p.oauthContext(ctx), token), time.Minute)
	fresh, err := source.Token()
	if err != nil {
		return Credential{}, err
	}
	return credential(fresh), nil
}
func (p Provider) Identity(ctx context.Context, token string) (connector.Identity, error) {
	implementation, ok := registry.ByName(p.Name)
	if !ok {
		return connector.Identity{}, fmt.Errorf("unsupported provider %q", p.Name)
	}
	return implementation.Identity(ctx, p.get, token)
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
