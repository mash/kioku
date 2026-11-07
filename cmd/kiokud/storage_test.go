package main

import (
	"context"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/blob/local"
	"github.com/mash/kioku/internal/blob/s3"
	"testing"
)

func TestOpenStorageEnvironment(t *testing.T) {
	values := map[string]string{}
	getenv := func(k string) string { return values[k] }
	if _, err := openStorage("", t.TempDir(), getenv); err == nil {
		t.Fatal("missing backend accepted")
	}
	objects, err := openStorage(blob.BackendLocal, t.TempDir(), getenv)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := objects.(local.Store); !ok {
		t.Fatalf("unexpected local backend %T", objects)
	}
	values = map[string]string{"S3_ENDPOINT": "https://storage.example.com", "S3_REGION": "eu-west-1", "S3_BUCKET": "kioku-projects", "S3_ACCESS_KEY_ID": "test", "S3_SECRET_ACCESS_KEY": "secret", "S3_SESSION_TOKEN": "session"}
	objects, err = openStorage(blob.BackendS3, t.TempDir(), getenv)
	if err != nil {
		t.Fatal(err)
	}
	remote, ok := objects.(*s3.Projects)
	if !ok {
		t.Fatal("unexpected backend")
	}
	options := remote.Template.Client.Options()
	creds, err := options.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if options.Region != "eu-west-1" || creds.SessionToken != "session" {
		t.Fatal("S3 configuration not forwarded")
	}
	if _, err = openStorage(blob.BackendLocal, t.TempDir(), getenv); err == nil {
		t.Fatal("mixed backend settings accepted")
	}
	delete(values, "S3_REGION")
	if _, err = openStorage(blob.BackendS3, t.TempDir(), getenv); err == nil {
		t.Fatal("incomplete S3 settings accepted")
	}
	if _, err = openStorage("unknown", t.TempDir(), getenv); err == nil {
		t.Fatal("unknown backend accepted")
	}
}
