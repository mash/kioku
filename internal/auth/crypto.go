package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

func Token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func Hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func Challenge(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

type Cipher struct{ aead cipher.AEAD }

func NewCipher(key string) (*Cipher, error) {
	b, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(b) != 32 {
		return nil, errors.New("KIOKU_ENCRYPTION_KEY must be a base64-encoded 32-byte key")
	}
	block, err := aes.NewCipher(b)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(block)
	return &Cipher{a}, err
}
func (c *Cipher) Seal(value, context string) string {
	n := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(n); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(c.aead.Seal(n, n, []byte(value), []byte(context)))
}
func (c *Cipher) Open(value, context string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	n := c.aead.NonceSize()
	if len(b) < n {
		return "", errors.New("invalid encrypted credential")
	}
	v, err := c.aead.Open(nil, b[:n], b[n:], []byte(context))
	return string(v), err
}
