package main

import (
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/blob/s3"
)

// openStorage translates environment configuration once at startup.
func openStorage(backend blob.Backend, dataDir string, getenv func(string) string) (blob.Store, error) {
	c := storageConfig{Backend: backend}
	remote := s3.Config{
		Endpoint: getenv("S3_ENDPOINT"), Region: getenv("S3_REGION"), Bucket: getenv("S3_BUCKET"),
		AccessKeyID: getenv("S3_ACCESS_KEY_ID"), SecretAccessKey: getenv("S3_SECRET_ACCESS_KEY"), SessionToken: getenv("S3_SESSION_TOKEN"),
	}
	if backend == blob.BackendLocal {
		c.Local = &local.Config{DataDir: dataDir}
	}
	if backend == blob.BackendS3 || remote != (s3.Config{}) {
		c.S3 = &remote
	}
	return openStore(c)
}
