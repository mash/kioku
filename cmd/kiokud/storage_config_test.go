package main

import (
	"context"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mash/kioku/internal/blob/s3"
)

func TestOpenLocal(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(storageConfig{Backend: blob.BackendLocal, Local: &local.Config{DataDir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	key := blob.Key("projects/p/wiki/index.md")
	ctx := context.Background()
	if err = s.Put(ctx, key, []byte("wiki")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "objects", string(key)))
	if err != nil || string(b) != "wiki" {
		t.Fatalf("local persistence: %q, %v", b, err)
	}
	reopened, err := openStore(storageConfig{Backend: blob.BackendLocal, Local: &local.Config{DataDir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	b, err = reopened.Get(ctx, key)
	if err != nil || string(b) != "wiki" {
		t.Fatalf("reopen: %q, %v", b, err)
	}
}

func TestOpenS3(t *testing.T) {
	unavailable := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer unavailable.Close()
	store, err := openStore(storageConfig{Backend: blob.BackendS3, S3: &s3.Config{Endpoint: unavailable.URL, Region: "auto", Bucket: "kioku-projects", AccessKeyID: "test", SecretAccessKey: "test", HTTPClient: unavailable.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*s3.Projects); !ok {
		t.Fatalf("unexpected backend %T", store)
	}
	if err := store.Put(context.Background(), "projects/p/wiki/index.md", []byte("wiki")); err == nil {
		t.Fatal("S3 failure must not fall back to local storage")
	}
}

func TestOpenRejectsInvalidConfiguration(t *testing.T) {
	cases := []storageConfig{
		{}, {Backend: "unknown"}, {Backend: blob.BackendLocal}, {Backend: blob.BackendS3},
		{Backend: blob.BackendLocal, Local: &local.Config{}},
		{Backend: blob.BackendS3, S3: &s3.Config{}},
		{Backend: blob.BackendLocal, Local: &local.Config{DataDir: t.TempDir()}, S3: &s3.Config{}},
		{Backend: blob.BackendS3, Local: &local.Config{DataDir: t.TempDir()}, S3: &s3.Config{}},
	}
	for _, c := range cases {
		if s, err := openStore(c); err == nil || s != nil {
			t.Fatalf("accepted invalid config for %q: %T, %v", c.Backend, s, err)
		}
	}
}
