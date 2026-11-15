package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestSealedOAuthCookieChecksStateProviderAndExpiry(t *testing.T) {
	c, err := NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	payload := OAuthCookie{Provider: "google", Verifier: "private-verifier", Expires: 100}
	cookie, err := c.SealOAuthCookie(payload)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.OpenOAuthCookie(cookie, Hash(cookie), "google", 99)
	if err != nil || got.Verifier != payload.Verifier {
		t.Fatal("valid cookie rejected", err)
	}
	for _, tc := range []struct {
		cookie, state, provider string
		now                     int64
	}{{cookie, "bad", "google", 99}, {cookie, Hash(cookie), "github", 99}, {cookie, Hash(cookie), "google", 100}, {cookie + "bad", Hash(cookie + "bad"), "google", 99}} {
		if _, err := c.OpenOAuthCookie(tc.cookie, tc.state, tc.provider, tc.now); err == nil {
			t.Fatal("invalid cookie accepted")
		}
	}
	// Cookies have no server-side state. Provider code exchange decides replay.
	if _, err := c.OpenOAuthCookie(cookie, Hash(cookie), "google", 99); err != nil {
		t.Fatal(err)
	}
}
func TestSealedJobCredentialIsBoundToItsJobAndDeadline(t *testing.T) {
	c, err := NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.SealJobCredential(JobCredential{JobID: "j1", Attempt: "j1", Expires: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "kioku_agent.j1.") {
		t.Fatal("token shape changed")
	}
	if _, err := c.OpenJobCredential(raw, 99); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{raw, strings.Replace(raw, ".j1.", ".j2.", 1), raw + "bad", "kioku_agent.j1."} {
		if _, err := c.OpenJobCredential(raw, 100); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
}
func TestIdentityTokensUseSeparateContextsAndNulls(t *testing.T) {
	c, err := NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	original := Credential{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.UnixMilli(1234).UTC()}
	sealed := c.SealIdentityTokens(original, "u", "google", "subject")
	got, err := c.OpenIdentityTokens(sealed, "u", "google", "subject")
	if err != nil || got != original {
		t.Fatal("round trip", err)
	}
	if _, err := c.OpenIdentityTokens(sealed, "another", "google", "subject"); err == nil {
		t.Fatal("wrong user accepted")
	}
	sealed.AccessToken, sealed.RefreshToken = sealed.RefreshToken, sealed.AccessToken
	if _, err := c.OpenIdentityTokens(sealed, "u", "google", "subject"); err == nil {
		t.Fatal("token contexts interchanged")
	}
	if got := c.SealIdentityTokens(Credential{}, "u", "google", "subject"); got.AccessToken != nil || got.RefreshToken != nil || got.ExpiresAt != nil {
		t.Fatal("absence not null")
	}
}
