package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mash/kioku/internal/atomicfile"
	"github.com/mash/kioku/internal/auth"
)

// loadCipherForStorage runs under the catalog process lock.
func loadCipherForStorage(dataDir, explicitKey string, create bool) (*auth.Cipher, error) {
	if explicitKey != "" {
		return auth.NewCipher(explicitKey)
	}
	path := filepath.Join(dataDir, "encryption.key")
	b, err := os.ReadFile(path)
	if err == nil {
		cipher, err := auth.NewCipher(strings.TrimSpace(string(b)))
		if err != nil {
			return nil, fmt.Errorf("invalid encryption key file %s: expected a base64-encoded 32-byte key", path)
		}
		return cipher, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read encryption key: %w", err)
	}

	if !create {
		return nil, fmt.Errorf("existing catalog encryption key is missing: restore the matching key")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate encryption key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	cipher, err := auth.NewCipher(encoded)
	if err != nil {
		return nil, err
	}
	// Persist before use so credentials and OAuth signatures survive restarts.
	if err := atomicfile.Write(path, []byte(encoded+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("save encryption key: %w", err)
	}
	return cipher, nil
}
