package s3

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentInputObjectReadLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), MaxObject+1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
	defer ts.Close()
	s, err := New(Config{Endpoint: ts.URL, Bucket: "test", Region: "auto", AccessKeyID: "test", SecretAccessKey: "test", HTTPClient: ts.Client()})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []Key{"projects/p/sources/s/extracted.txt", "projects/p/.kioku/jobs/j/output-hash.tar.gz", "projects/p/.kioku/jobs/j/input.tar.gz"} {
		b, err := s.Get(context.Background(), key)
		if strings.HasSuffix(string(key), "/input.tar.gz") {
			if err != nil || len(b) != len(payload) {
				t.Fatal(len(b), err)
			}
		} else if err == nil {
			t.Fatal("ordinary object limit bypass", key)
		}
	}
}
