package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/mash/kioku/internal/blob"
)

const MaxObject = 16 << 20

type Key = blob.Key

// Store delegates transport, signing and retries to the AWS SDK for Go v2.
type Store struct {
	Bucket string
	Client *awss3.Client
}

func (s Store) Put(ctx context.Context, key Key, body []byte) error {
	if !blob.ValidKey(key) {
		return errors.New("invalid object key")
	}
	_, err := s.Client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(string(key)), Body: bytes.NewReader(body)})
	return storageError(err)
}
func (s Store) Get(ctx context.Context, key Key) ([]byte, error) {
	if !blob.ValidKey(key) {
		return nil, errors.New("invalid object key")
	}
	out, err := s.Client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(string(key))})
	if err != nil {
		return nil, storageError(err)
	}
	defer out.Body.Close()
	limit := objectReadLimit(key)
	if out.ContentLength != nil && *out.ContentLength > limit {
		return nil, errors.New("object too large")
	}
	b, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("object too large")
	}
	return b, err
}
func (s Store) Delete(ctx context.Context, key Key) error {
	if !blob.ValidKey(key) {
		return errors.New("invalid object key")
	}
	_, err := s.Client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(string(key))})
	return storageError(err)
}
func (s Store) List(ctx context.Context, prefix string) ([]Key, error) {
	pages := awss3.NewListObjectsV2Paginator(s.Client, &awss3.ListObjectsV2Input{Bucket: aws.String(s.Bucket), Prefix: aws.String(prefix)})
	keys := []Key{}
	previous := ""
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, storageError(err)
		}
		for _, object := range page.Contents {
			keys = append(keys, Key(aws.ToString(object.Key)))
		}
		if aws.ToBool(page.IsTruncated) {
			next := aws.ToString(page.NextContinuationToken)
			if next == "" || next == previous {
				return nil, errors.New("invalid storage pagination")
			}
			previous = next
		}
	}
	return keys, nil
}

type notFoundError struct{ err error }

func (e *notFoundError) Error() string  { return e.err.Error() }
func (e *notFoundError) Unwrap() error  { return e.err }
func (e *notFoundError) NotFound() bool { return true }
func storageError(err error) error {
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) && response.HTTPStatusCode() == 404 {
		return &notFoundError{err: err}
	}
	return err
}

// Job input contains all sources. Allow gzip overhead above its 256 MiB tar
// limit without broadening the limit on source, metadata or output objects.
func objectReadLimit(key Key) int64 {
	p := strings.Split(string(key), "/")
	if len(p) == 6 && p[0] == "projects" && p[2] == ".kioku" && p[3] == "jobs" && p[5] == "input.tar.gz" {
		return 257 << 20
	}
	return MaxObject
}
