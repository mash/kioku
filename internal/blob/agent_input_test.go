package blob

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
	s := S3{Endpoint: ts.URL, Bucket: "test", Region: "auto", AccessKey: "test", SecretKey: "test", Client: ts.Client()}
	for _, key := range []string{"projects/p/sources/s/extracted.txt", "projects/p/.kioku/jobs/j/output-hash.tar.gz", "projects/p/.kioku/jobs/j/input.tar.gz"} {
		b, err := s.Get(context.Background(), key)
		if strings.HasSuffix(key, "/input.tar.gz") {
			if err != nil || len(b) != len(payload) {
				t.Fatal(len(b), err)
			}
		} else if err == nil {
			t.Fatal("ordinary object limit bypass", key)
		}
	}
}
