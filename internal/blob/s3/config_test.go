package s3

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"testing"
)

func TestConfigUsesEndpointAndRegion(t *testing.T) {
	for _, region := range []string{"auto", "eu-west-1"} {
		c := Config{Endpoint: "https://storage.example.com/", Region: region, Bucket: "kioku-projects", AccessKeyID: "test", SecretAccessKey: "secret", SessionToken: "session"}
		s, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		options := s.Template.Client.Options()
		creds, err := options.Credentials.Retrieve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(options.BaseEndpoint) != "https://storage.example.com" || options.Region != region || creds.SessionToken != "session" || !options.UsePathStyle {
			t.Fatal("configuration not preserved")
		}
	}
}

func TestConfigRejectsInvalidSettings(t *testing.T) {
	base := Config{Endpoint: "https://storage.example.com", Region: "auto", Bucket: "kioku-projects", AccessKeyID: "test", SecretAccessKey: "secret"}
	for _, change := range []func(*Config){
		func(c *Config) { c.Endpoint = "" },
		func(c *Config) { c.Endpoint = "ftp://storage.example.com" },
		func(c *Config) { c.Endpoint = "https://user:secret@storage.example.com" },
		func(c *Config) { c.Endpoint = "https://storage.example.com/path" },
		func(c *Config) { c.Endpoint = "https://storage.example.com?query=1" },
		func(c *Config) { c.Region = "" },
		func(c *Config) { c.Region = "bad/region" },
		func(c *Config) { c.Bucket = "../bucket" },
		func(c *Config) { c.AccessKeyID = "" },
		func(c *Config) { c.SecretAccessKey = "" },
	} {
		c := base
		change(&c)
		if s, err := New(c); err == nil || s != nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
