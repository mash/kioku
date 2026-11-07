// Package blob defines the backend-independent storage contract.
package blob

import (
	"context"
	"path"
	"strings"
)

// Key identifies an object within the selected storage backend.
type Key string

type Store interface {
	Put(context.Context, Key, []byte) error
	Get(context.Context, Key) ([]byte, error)
	Delete(context.Context, Key) error
	List(context.Context, string) ([]Key, error)
}

func ValidKey(value Key) bool {
	key := string(value)
	return key != "" && !strings.HasPrefix(key, "/") && !strings.ContainsAny(key, "\\\x00") && path.Clean(key) == key && key != ".." && !strings.HasPrefix(key, "../")
}

type Backend string

const (
	BackendLocal Backend = "local"
	BackendS3    Backend = "s3"
)
