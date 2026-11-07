// Package githubapp implements the github.com-only, read-only source App.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mash/kioku/internal/runner"
)

const APIVersion = "2026-03-10"

type Config struct {
	Enabled        bool
	ID             int64
	ClientID, Slug string
	ClientSecret   runner.Secret
	PrivateKey     *rsa.PrivateKey
}

func (c Config) Configured() bool {
	return c.ID > 0 && c.ClientID != "" && c.Slug != "" && c.ClientSecret != "" && c.PrivateKey != nil
}

func LoadConfig(get func(string) string) (Config, error) {
	var c Config
	enabled := get("KIOKU_GITHUB_APP_ENABLED")
	if enabled != "" && enabled != "false" && enabled != "true" {
		return c, errors.New("invalid GitHub App enabled setting")
	}
	c.Enabled = enabled == "true"
	keys := []string{"ID", "CLIENT_ID", "SLUG", "CLIENT_SECRET", "PRIVATE_KEY_FILE"}
	values := make([]string, len(keys))
	count := 0
	for i, k := range keys {
		values[i] = get("KIOKU_GITHUB_APP_" + k)
		if values[i] != "" {
			count++
		}
	}
	if count == 0 && !c.Enabled {
		return c, nil
	}
	invalid := errors.New("GitHub App requires complete settings and a valid absolute 0600 RSA private key file")
	if count != len(keys) {
		return c, invalid
	}
	c.ID, _ = strconv.ParseInt(values[0], 10, 64)
	c.ClientID, c.Slug, c.ClientSecret = values[1], values[2], runner.Secret(values[3])
	if c.ID <= 0 || !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(c.ClientID) || !regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`).MatchString(c.Slug) || !filepath.IsAbs(values[4]) {
		return Config{}, invalid
	}
	info, err := os.Stat(values[4])
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return Config{}, invalid
	}
	b, err := os.ReadFile(values[4])
	if err != nil {
		return Config{}, invalid
	}
	block, rest := pem.Decode(b)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return Config{}, invalid
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		c.PrivateKey = key
	} else if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		c.PrivateKey, _ = key.(*rsa.PrivateKey)
	}
	if c.PrivateKey == nil || c.PrivateKey.N.BitLen() < 2048 || c.PrivateKey.Validate() != nil {
		return Config{}, invalid
	}
	return c, nil
}

func (c Config) JWT(now time.Time) (runner.Secret, time.Time, error) {
	if !c.Configured() {
		return "", time.Time{}, &Failure{Reason: "configuration_error"}
	}
	expiry := now.Add(9 * time.Minute)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": expiry.Unix(), "iss": c.ClientID})
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.PrivateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", expiry, &Failure{Reason: "configuration_error"}
	}
	return runner.Secret(unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)), expiry, nil
}

// Failure discards response bodies, URLs, credentials and transport errors.
// Unknown means a non-idempotent request might have been accepted remotely.
type Failure struct {
	Status             int
	Reason             string
	Unknown, Secondary bool
	RetryAt            time.Time
}

func (f *Failure) Error() string { return "GitHub App: " + f.Reason }
func Reason(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Reason
	}
	return "temporary"
}

type Observation struct {
	Operation, Class, Reason, Resource string
	Remaining                          int64
}
type Client struct {
	Config  Config
	HTTP    *http.Client
	Observe func(Observation)
	Now     func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}
func (c *Client) request(ctx context.Context, op, method, endpoint string, token runner.Secret, body any, result any) (http.Header, error) {
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, &Failure{Reason: "configuration_error"}
		}
		input = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, input)
	if err != nil {
		return nil, &Failure{Reason: "configuration_error"}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+string(token))
	}
	client := http.Client{Timeout: 10 * time.Second}
	if c.HTTP != nil {
		client = *c.HTTP
		client.Timeout = 10 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// A POST body has no GetBody: net/http must not replay issuance/refresh.
	if method == "POST" {
		req.GetBody = nil
	}
	resp, err := client.Do(req)
	obs := Observation{Operation: op, Class: "transport", Reason: "temporary", Remaining: -1}
	defer func() {
		if c.Observe != nil {
			c.Observe(obs)
		}
	}()
	if err != nil {
		return nil, &Failure{Reason: "temporary", Unknown: method == "POST"}
	}
	defer resp.Body.Close()
	obs.Class = strconv.Itoa(resp.StatusCode/100) + "xx"
	obs.Resource = "installation"
	if op == "user" || op == "installations" || op == "repositories" {
		obs.Resource = "user"
	}
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil {
			obs.Remaining = n
		}
	}
	b, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || readErr != nil {
		f := &Failure{Status: resp.StatusCode, Reason: "temporary", Unknown: method == "POST" && (resp.StatusCode >= 500 || resp.StatusCode < 400 || readErr != nil)}
		var message struct{ Message string }
		_ = json.Unmarshal(b, &message)
		f.Secondary = strings.Contains(strings.ToLower(message.Message), "secondary rate") || strings.Contains(strings.ToLower(message.Message), "abuse detection")
		switch {
		case resp.StatusCode == 429 || obs.Remaining == 0 || f.Secondary:
			f.Reason = "rate_limited"
		case resp.StatusCode == 401:
			f.Reason = "unauthorized"
		case resp.StatusCode == 403:
			f.Reason = "forbidden"
		case resp.StatusCode == 404:
			f.Reason = "missing_or_inaccessible"
		}
		if v := resp.Header.Get("Retry-After"); v != "" {
			if n, e := strconv.Atoi(v); e == nil && n >= 0 {
				f.RetryAt = c.now().Add(time.Duration(n) * time.Second)
			} else if t, e := http.ParseTime(v); e == nil {
				f.RetryAt = t
			}
		}
		if obs.Remaining == 0 {
			if n, e := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); e == nil && time.Unix(n, 0).After(f.RetryAt) {
				f.RetryAt = time.Unix(n, 0)
			}
		}
		obs.Reason = f.Reason
		return resp.Header, f
	}
	if result != nil && json.Unmarshal(b, result) != nil {
		return resp.Header, &Failure{Reason: "temporary", Unknown: method == "POST"}
	}
	obs.Reason = "success"
	return resp.Header, nil
}

type UserCredential struct {
	AccessToken, RefreshToken         string
	AccessExpiresAt, RefreshExpiresAt time.Time
}

func (UserCredential) String() string { return "[redacted]" }
func (c *Client) Exchange(ctx context.Context, code, refresh string) (UserCredential, error) {
	body := map[string]string{"client_id": c.Config.ClientID, "client_secret": string(c.Config.ClientSecret)}
	op := "exchange"
	if refresh != "" {
		body["grant_type"] = "refresh_token"
		body["refresh_token"] = refresh
		op = "refresh"
	} else {
		body["code"] = code
	}
	var out struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
		Error            string `json:"error"`
	}
	_, err := c.request(ctx, op, "POST", "https://github.com/login/oauth/access_token", "", body, &out)
	if err != nil {
		return UserCredential{}, err
	}
	if out.Error != "" || out.AccessToken == "" || out.RefreshToken == "" || out.ExpiresIn <= 0 || out.RefreshExpiresIn <= 0 {
		return UserCredential{}, &Failure{Reason: "reauthorization_required"}
	}
	return UserCredential{out.AccessToken, out.RefreshToken, c.now().Add(time.Duration(out.ExpiresIn) * time.Second), c.now().Add(time.Duration(out.RefreshExpiresIn) * time.Second)}, nil
}

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}
type Installation struct {
	ID          int64             `json:"id"`
	AppID       int64             `json:"app_id"`
	Account     User              `json:"account"`
	SuspendedAt *time.Time        `json:"suspended_at"`
	Permissions map[string]string `json:"permissions"`
}
type Repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

func (c *Client) User(ctx context.Context, token runner.Secret) (User, error) {
	var out User
	_, err := c.request(ctx, "user", "GET", "https://api.github.com/user", token, nil, &out)
	if err == nil && (out.ID <= 0 || out.Login == "") {
		err = &Failure{Reason: "inaccessible"}
	}
	return out, err
}
func (c *Client) Installations(ctx context.Context, token runner.Secret) ([]Installation, error) {
	var all []Installation
	seen := map[int64]bool{}
	for page := 1; ; page++ {
		var out struct {
			Installations []Installation `json:"installations"`
		}
		h, err := c.request(ctx, "installations", "GET", fmt.Sprintf("https://api.github.com/user/installations?per_page=100&page=%d", page), token, nil, &out)
		if err != nil {
			return nil, err
		}
		for _, v := range out.Installations {
			if v.ID <= 0 || seen[v.ID] {
				return nil, &Failure{Reason: "inaccessible"}
			}
			seen[v.ID] = true
			all = append(all, v)
		}
		if !strings.Contains(h.Get("Link"), `rel="next"`) && len(out.Installations) < 100 {
			return all, nil
		}
	}
}
func (c *Client) Repositories(ctx context.Context, token runner.Secret, id int64) ([]Repository, error) {
	if id <= 0 {
		return nil, &Failure{Reason: "configuration_error"}
	}
	var all []Repository
	seen := map[int64]bool{}
	for page := 1; ; page++ {
		var out struct {
			Repositories []Repository `json:"repositories"`
		}
		h, err := c.request(ctx, "repositories", "GET", fmt.Sprintf("https://api.github.com/user/installations/%d/repositories?per_page=100&page=%d", id, page), token, nil, &out)
		if err != nil {
			return nil, err
		}
		for _, v := range out.Repositories {
			if v.ID <= 0 || seen[v.ID] || len(strings.Split(v.FullName, "/")) != 2 {
				return nil, &Failure{Reason: "inaccessible"}
			}
			seen[v.ID] = true
			all = append(all, v)
		}
		if !strings.Contains(h.Get("Link"), `rel="next"`) && len(out.Repositories) < 100 {
			return all, nil
		}
	}
}
func (c *Client) Installation(ctx context.Context, id int64) (Installation, error) {
	var out Installation
	jwt, _, err := c.Config.JWT(c.now())
	if err != nil {
		return out, err
	}
	_, err = c.request(ctx, "installation", "GET", fmt.Sprintf("https://api.github.com/app/installations/%d", id), jwt, nil, &out)
	if Reason(err) == "unauthorized" {
		err = &Failure{Reason: "configuration_error"}
	}
	return out, err
}
func Permissions() map[string]string {
	return map[string]string{"contents": "read", "metadata": "read", "issues": "read"}
}
func ValidateInstallation(v Installation, app, account int64) error {
	if v.ID <= 0 || v.AppID != app || v.Account.ID != account {
		return &Failure{Reason: "inaccessible"}
	}
	if v.SuspendedAt != nil {
		return &Failure{Reason: "suspended"}
	}
	for k := range Permissions() {
		if v.Permissions[k] != "read" {
			return &Failure{Reason: "configuration_error"}
		}
	}
	return nil
}

type AuthorizedRepositorySet struct {
	OwnerID           string
	ConnectionVersion uint64
	InstallationID    int64
	RepositoryIDs     []int64
	CheckedAt         time.Time
}

func (s AuthorizedRepositorySet) Validate() error {
	if s.OwnerID == "" || s.InstallationID <= 0 || len(s.RepositoryIDs) == 0 || len(s.RepositoryIDs) > 500 {
		return &Failure{Reason: "configuration_error"}
	}
	seen := map[int64]bool{}
	for _, id := range s.RepositoryIDs {
		if id <= 0 || seen[id] {
			return &Failure{Reason: "configuration_error"}
		}
		seen[id] = true
	}
	return nil
}
func (c *Client) Issue(ctx context.Context, jwt runner.Secret, scope AuthorizedRepositorySet) (runner.Secret, time.Time, error) {
	if err := scope.Validate(); err != nil {
		return "", time.Time{}, err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	_, err := c.request(ctx, "issue", "POST", fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", scope.InstallationID), jwt, map[string]any{"repository_ids": scope.RepositoryIDs, "permissions": Permissions()}, &out)
	if err != nil {
		return "", time.Time{}, err
	}
	if out.Token == "" || !out.ExpiresAt.After(c.now()) {
		return "", time.Time{}, &Failure{Reason: "issuance_unknown", Unknown: true}
	}
	return runner.Secret(out.Token), out.ExpiresAt, nil
}
func (c *Client) Revoke(ctx context.Context, token runner.Secret) error {
	_, err := c.request(ctx, "revoke", "DELETE", "https://api.github.com/installation/token", token, nil, nil)
	return err
}
