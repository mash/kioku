package auth

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestCredentialEncryptionIsBoundToUserAndProvider(t *testing.T) {
	c, err := NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	sealed := c.Seal("provider-secret", "alice:google")
	if strings.Contains(sealed, "provider-secret") {
		t.Fatal("plaintext")
	}
	if got, err := c.Open(sealed, "alice:google"); err != nil || got != "provider-secret" {
		t.Fatal(got, err)
	}
	if _, err = c.Open(sealed, "bob:google"); err == nil {
		t.Fatal("cross-user credential accepted")
	}
	if _, err = c.Open(sealed, "alice:github"); err == nil {
		t.Fatal("cross-provider credential accepted")
	}
}

func TestOAuthSecretIsStableAndSeparated(t *testing.T) {
	raw := make([]byte, 32)
	key := base64.StdEncoding.EncodeToString(raw)
	first, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	secret := first.OAuthSecret()
	if len(secret) != 32 || string(secret) == string(raw) || string(secret) != string(second.OAuthSecret()) {
		t.Fatal("invalid or unstable derived key")
	}
	secret[0] ^= 0xff
	if string(first.OAuthSecret()) != string(second.OAuthSecret()) {
		t.Fatal("caller mutated signing key")
	}
	raw[0] = 1
	other, err := NewCipher(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(first.OAuthSecret()) == string(other.OAuthSecret()) {
		t.Fatal("different encryption keys produced same signing key")
	}
}
