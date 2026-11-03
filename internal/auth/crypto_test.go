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
