package server

import (
	"encoding/json"

	"github.com/mash/kioku/internal/auth"
)

func snapshotToken(files map[string]string) string {
	b, _ := json.Marshal(files)
	return auth.Hash(string(b))[:32]
}
