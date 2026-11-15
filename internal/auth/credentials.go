package auth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrInvalidCredential = errors.New("invalid credential")

func (c *Cipher) SealOAuthCookie(v OAuthCookie) (string, error) {
	if v.Provider == "" || v.Verifier == "" || v.Expires <= 0 {
		return "", ErrInvalidCredential
	}
	type wire OAuthCookie
	b, err := json.Marshal(wire(v))
	if err != nil {
		return "", err
	}
	return c.Seal(string(b), "oauth-cookie"), nil
}
func (c *Cipher) OpenOAuthCookie(cookie, state, provider string, now int64) (OAuthCookie, error) {
	var v OAuthCookie
	if subtle.ConstantTimeCompare([]byte(Hash(cookie)), []byte(state)) != 1 {
		return v, ErrInvalidCredential
	}
	plain, err := c.Open(cookie, "oauth-cookie")
	if err != nil {
		return v, ErrInvalidCredential
	}
	if err = json.Unmarshal([]byte(plain), &v); err != nil || v.Provider != provider || v.Verifier == "" || now >= v.Expires {
		return OAuthCookie{}, ErrInvalidCredential
	}
	return v, nil
}
func (c *Cipher) SealJobCredential(v JobCredential) (string, error) {
	if v.JobID == "" || strings.ContainsAny(v.JobID, "./\\\x00") || v.Attempt != v.JobID || v.Expires <= 0 {
		return "", ErrInvalidCredential
	}
	type wire JobCredential
	b, err := json.Marshal(wire(v))
	if err != nil {
		return "", err
	}
	return "kioku_agent." + v.JobID + "." + c.Seal(string(b), "job:"+v.JobID), nil
}
func (c *Cipher) OpenJobCredential(token string, now int64) (JobCredential, error) {
	var v JobCredential
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "kioku_agent" || parts[1] == "" {
		return v, ErrInvalidCredential
	}
	plain, err := c.Open(parts[2], "job:"+parts[1])
	if err != nil {
		return v, ErrInvalidCredential
	}
	if err = json.Unmarshal([]byte(plain), &v); err != nil || v.JobID != parts[1] || v.Attempt != v.JobID || now >= v.Expires {
		return JobCredential{}, ErrInvalidCredential
	}
	return v, nil
}

type IdentityTokens struct {
	AccessToken  *string `json:"identity_access_token"`
	RefreshToken *string `json:"identity_refresh_token"`
	ExpiresAt    *int64  `json:"identity_expires_at"`
}

func identityTokenContext(user, provider, subject string) string {
	return "identity:" + user + ":" + provider + ":" + subject
}
func (c *Cipher) SealIdentityTokens(v Credential, user, provider, subject string) IdentityTokens {
	out := IdentityTokens{}
	context := identityTokenContext(user, provider, subject)
	if v.AccessToken != "" {
		s := c.Seal(v.AccessToken, context+":access_token")
		out.AccessToken = &s
	}
	if v.RefreshToken != "" {
		s := c.Seal(v.RefreshToken, context+":refresh_token")
		out.RefreshToken = &s
	}
	if !v.ExpiresAt.IsZero() {
		ms := v.ExpiresAt.UnixMilli()
		out.ExpiresAt = &ms
	}
	return out
}
func (c *Cipher) OpenIdentityTokens(v IdentityTokens, user, provider, subject string) (Credential, error) {
	out := Credential{}
	context := identityTokenContext(user, provider, subject)
	var err error
	if v.AccessToken != nil {
		out.AccessToken, err = c.Open(*v.AccessToken, context+":access_token")
		if err != nil {
			return Credential{}, ErrInvalidCredential
		}
	}
	if v.RefreshToken != nil {
		out.RefreshToken, err = c.Open(*v.RefreshToken, context+":refresh_token")
		if err != nil {
			return Credential{}, ErrInvalidCredential
		}
	}
	if v.ExpiresAt != nil {
		out.ExpiresAt = time.UnixMilli(*v.ExpiresAt).UTC()
	}
	return out, nil
}
