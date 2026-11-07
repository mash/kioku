package s3

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"

	"github.com/mash/kioku/internal/blob"
)

// Projects stores all project objects in one pre-provisioned private bucket.
// Object keys retain their projects/<id>/ prefix; bucket management is external.
type Projects struct {
	Template Store
	buckets  sync.Map
}

var sharedBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

func (p *Projects) Bucket(id string) string {
	if value, ok := p.buckets.Load(id); ok {
		return value.(string)
	}
	return p.Template.Bucket
}

func (p *Projects) EnsureProject(ctx context.Context, id string) error {
	if err := p.checkProject(id); err != nil {
		return err
	}
	// Verify object access without requiring bucket management permissions.
	_, err := p.Template.List(ctx, "projects/"+id+"/")
	return err
}

func (p *Projects) DeleteProject(_ context.Context, id string) error {
	// The caller removes project objects first. Never delete the shared bucket.
	return p.checkProject(id)
}

func (p *Projects) route(key Key) (Store, error) {
	parts := strings.SplitN(string(key), "/", 3)
	if len(parts) != 3 || parts[0] != "projects" || parts[1] == "" || !blob.ValidKey(key) {
		return Store{}, errors.New("project-scoped object key required")
	}
	if err := p.checkProject(parts[1]); err != nil {
		return Store{}, err
	}
	s := p.Template
	s.Bucket = p.Bucket(parts[1])
	return s, nil
}
func (p *Projects) Put(ctx context.Context, key Key, b []byte) error {
	s, err := p.route(key)
	if err != nil {
		return err
	}
	return s.Put(ctx, key, b)
}
func (p *Projects) Get(ctx context.Context, key Key) ([]byte, error) {
	s, err := p.route(key)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, key)
}
func (p *Projects) Delete(ctx context.Context, key Key) error {
	s, err := p.route(key)
	if err != nil {
		return err
	}
	return s.Delete(ctx, key)
}
func (p *Projects) List(ctx context.Context, prefix string) ([]Key, error) {
	s, err := p.route(Key(prefix + "placeholder"))
	if err != nil {
		return nil, err
	}
	keys, err := s.List(ctx, prefix)
	return keys, err
}

func (p *Projects) RegisterProjectBucket(id, bucket string) {
	if bucket != "" {
		p.buckets.Store(id, bucket)
	}
}

// Refuse silent migration of catalog entries that point to a legacy bucket.
func (p *Projects) checkProject(id string) error {
	if !blob.ValidKey(Key("projects/"+id)) || strings.Contains(id, "/") || id == "" {
		return errors.New("invalid project ID")
	}
	if !sharedBucketName.MatchString(p.Template.Bucket) {
		return errors.New("invalid S3_BUCKET")
	}
	if p.Bucket(id) != p.Template.Bucket {
		return errors.New("project bucket differs from S3_BUCKET; explicit migration required")
	}
	return nil
}
