// Package authtest provides signed Google OIDC fixtures for authentication tests.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

type Google struct{ key *rsa.PrivateKey }

func NewGoogle(t testing.TB) *Google {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Google{key: key}
}
func (g *Google) JWKS() jose.JSONWebKeySet {
	return jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &g.key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"}}}
}
func (g *Google) Claims(subject, verifier string) map[string]any {
	nonce := sha256.Sum256([]byte("kioku-oidc-nonce:" + verifier))
	return map[string]any{"iss": "https://accounts.google.com", "aud": "id", "sub": subject, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": base64.RawURLEncoding.EncodeToString(nonce[:])}
}
func (g *Google) Token(t testing.TB, subject, verifier string) string {
	return g.Sign(t, g.Claims(subject, verifier))
}
func (g *Google) Sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: g.key, KeyID: "test"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(body)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}
