package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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

func (p Provider) URL(state, verifier, purpose string) string {
	q := url.Values{"client_id": {p.ClientID}, "redirect_uri": {p.RedirectURL}, "state": {state}, "response_type": {"code"}, "code_challenge": {Challenge(verifier)}, "code_challenge_method": {"S256"}}
	base := "https://accounts.google.com/o/oauth2/v2/auth"
	scope := "openid email profile"
	if p.Name == "github" {
		base = "https://github.com/login/oauth/authorize"
		scope = "read:user user:email"
		if purpose == "connect" {
			scope += " repo"
		}
	} else if purpose == "connect" {
		scope += " https://www.googleapis.com/auth/documents.readonly https://www.googleapis.com/auth/drive.readonly"
		q.Set("access_type", "offline")
		q.Set("prompt", "consent")
	}
	q.Set("scope", scope)
	return base + "?" + q.Encode()
}
func (p Provider) Exchange(ctx context.Context, code, verifier string) (Credential, error) {
	return p.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {p.RedirectURL}})
}
func (p Provider) Refresh(ctx context.Context, c Credential) (Credential, error) {
	if c.ExpiresAt.IsZero() || time.Until(c.ExpiresAt) > time.Minute {
		return c, nil
	}
	if c.RefreshToken == "" {
		return c, errors.New("connection expired; reconnect the provider")
	}
	n, err := p.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.RefreshToken}})
	if n.RefreshToken == "" {
		n.RefreshToken = c.RefreshToken
	}
	return n, err
}
func (p Provider) token(ctx context.Context, q url.Values) (Credential, error) {
	q.Set("client_id", p.ClientID)
	q.Set("client_secret", p.ClientSecret)
	u := "https://oauth2.googleapis.com/token"
	if p.Name == "github" {
		u = "https://github.com/login/oauth/access_token"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(q.Encode()))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	if err = p.doJSON(req, &r); err != nil {
		return Credential{}, err
	}
	if r.Error != "" || r.AccessToken == "" {
		return Credential{}, errors.New("OAuth token exchange rejected")
	}
	c := Credential{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken}
	if r.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	return c, nil
}
func (p Provider) Identity(ctx context.Context, token string) (Identity, error) {
	if p.Name == "google" {
		var r struct {
			Sub      string `json:"sub"`
			Name     string `json:"name"`
			Email    string `json:"email"`
			Verified bool   `json:"email_verified"`
		}
		if err := p.get(ctx, "https://openidconnect.googleapis.com/v1/userinfo", token, &r); err != nil {
			return Identity{}, err
		}
		if r.Sub == "" || !r.Verified {
			return Identity{}, errors.New("a verified Google email is required")
		}
		return Identity{r.Sub, r.Name, strings.ToLower(r.Email)}, nil
	}
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := p.get(ctx, "https://api.github.com/user", token, &u); err != nil {
		return Identity{}, err
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := p.get(ctx, "https://api.github.com/user/emails", token, &emails); err != nil {
		return Identity{}, err
	}
	if u.ID == 0 {
		return Identity{}, errors.New("invalid GitHub user")
	}
	if u.Name == "" {
		u.Name = u.Login
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			return Identity{fmt.Sprint(u.ID), u.Name, strings.ToLower(e.Email)}, nil
		}
	}
	return Identity{}, errors.New("a verified primary GitHub email is required")
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
