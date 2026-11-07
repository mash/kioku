package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestGeneratedEncryptionKeySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	first, err := loadCipher(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "encryption.key")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("generated key must be private", err)
	}
	credential := first.Seal("test credential", "provider:user")
	second, err := loadCipher(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	value, err := second.Open(credential, "provider:user")
	if err != nil || value != "test credential" {
		t.Fatal("cannot decrypt credential after restart", err)
	}
	if !bytes.Equal(first.OAuthSecret(), second.OAuthSecret()) {
		t.Fatal("OAuth signing secret changed after restart")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("persisted key changed", err)
	}
	other, err := loadCipher(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(credential, "provider:user"); err == nil {
		t.Fatal("independent installations share a key")
	}
}

func TestExplicitEncryptionKeyTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	explicit, err := loadCipher(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "encryption.key")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("explicit key must not be copied to disk", err)
	}
	if _, err := loadCipher(dir, ""); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	override, err := loadCipher(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(explicit.OAuthSecret(), override.OAuthSecret()) {
		t.Fatal("explicit key did not override generated key")
	}
	if _, err := loadCipher(dir, "invalid explicit key"); err == nil {
		t.Fatal("invalid explicit key fell back to the key file")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("explicit key replaced persisted key", err)
	}
}

func TestEncryptionKeyFileFailureDoesNotGenerateReplacement(t *testing.T) {
	for _, body := range []string{"", "invalid key"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "encryption.key")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadCipher(dir, ""); err == nil {
			t.Fatal("invalid persisted key accepted")
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != body {
			t.Fatal("invalid persisted key was replaced", err)
		}
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "encryption.key"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCipher(dir, ""); err == nil {
		t.Fatal("unreadable key file accepted")
	}
	if _, err := loadCipher(filepath.Join(t.TempDir(), "missing"), ""); err == nil {
		t.Fatal("used a generated key that could not be persisted")
	}
}
