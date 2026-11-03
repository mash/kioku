package blob

import (
	"errors"
	"regexp"
)

var r2AccountID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// NewR2Projects uses Cloudflare's S3-compatible API, with no configurable AWS endpoint or region.
func NewR2Projects(account, bucket, access, secret string) (*Projects, error) {
	if !r2AccountID.MatchString(account) || !sharedBucketName.MatchString(bucket) || access == "" || secret == "" {
		return nil, errors.New("valid R2_ACCOUNT_ID, R2_BUCKET, R2_ACCESS_KEY_ID and R2_SECRET_ACCESS_KEY are required")
	}
	return &Projects{Template: S3{
		Endpoint: "https://" + account + ".r2.cloudflarestorage.com",
		Region:   "auto", Bucket: bucket, AccessKey: access, SecretKey: secret,
	}}, nil
}
