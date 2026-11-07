package main

import (
	"fmt"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/blob/s3"
)

// storageConfig selects exactly one durable store. The inactive backend must be nil.
// Invalid configuration or unavailable storage never triggers a fallback.
type storageConfig struct {
	Backend blob.Backend
	Local   *local.Config
	S3      *s3.Config
}

func openStore(c storageConfig) (blob.Store, error) {
	switch c.Backend {
	case blob.BackendLocal:
		if c.Local == nil || c.Local.DataDir == "" || c.S3 != nil {
			return nil, fmt.Errorf("local backend requires LocalConfig only, with a data directory")
		}
		return local.New(*c.Local)
	case blob.BackendS3:
		if c.S3 == nil || c.Local != nil {
			return nil, fmt.Errorf("s3 backend requires S3Config only")
		}
		remote, err := s3.New(*c.S3)
		if err != nil {
			return nil, err
		}
		return remote, nil
	default:
		return nil, fmt.Errorf("invalid storage backend %q: choose local or s3", c.Backend)
	}
}
