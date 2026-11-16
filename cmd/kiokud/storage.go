package main

import (
	"errors"
	"fmt"

	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/blob/s3"
)

// openStorage selects exactly one durable store from the environment once at startup.
// Invalid configuration or unavailable storage never triggers a fallback.
func openStorage(backend blob.Backend, dataDir string, getenv func(string) string) (blob.Store, error) {
	remote := s3.Config{
		Endpoint: getenv("S3_ENDPOINT"), Region: getenv("S3_REGION"), Bucket: getenv("S3_BUCKET"),
		AccessKeyID: getenv("S3_ACCESS_KEY_ID"), SecretAccessKey: getenv("S3_SECRET_ACCESS_KEY"), SessionToken: getenv("S3_SESSION_TOKEN"),
	}
	switch backend {
	case blob.BackendLocal:
		if remote != (s3.Config{}) {
			return nil, errors.New("local backend does not accept S3 settings")
		}
		return local.New(local.Config{DataDir: dataDir})
	case blob.BackendS3:
		s, err := s3.New(remote)
		if err != nil {
			return nil, err
		}
		return s, nil
	default:
		return nil, fmt.Errorf("invalid storage backend %q: choose local or s3", backend)
	}
}
