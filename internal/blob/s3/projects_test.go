package s3

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestProjectBucketIsolationAndLifecycle(t *testing.T) {
	var mu sync.Mutex
	buckets := map[string]map[string][]byte{}
	buckets["kioku-projects"] = map[string][]byte{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test/") || !strings.Contains(r.Header.Get("Authorization"), "/auto/s3/aws4_request") || r.Header.Get("X-Amz-Security-Token") != "session" {
			t.Error("missing signed request")
		}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		bucket := parts[0]
		key := ""
		if len(parts) == 2 {
			key = parts[1]
		}
		objects, exists := buckets[bucket]
		if key == "" {
			if r.Method != "GET" {
				t.Errorf("bucket administration attempted: %s", r.Method)
				w.WriteHeader(403)
				return
			}
			if !exists {
				w.WriteHeader(404)
				return
			}
			type item struct {
				Key string `xml:"Key"`
			}
			result := struct {
				XMLName  xml.Name `xml:"ListBucketResult"`
				Contents []item   `xml:"Contents"`
			}{}
			for k := range objects {
				if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
					result.Contents = append(result.Contents, item{k})
				}
			}
			_ = xml.NewEncoder(w).Encode(result)
			return
		}
		if !exists {
			w.WriteHeader(404)
			return
		}
		switch r.Method {
		case "PUT":
			b, _ := io.ReadAll(r.Body)
			objects[key] = b
		case "GET":
			b, ok := objects[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write(b)
		case "DELETE":
			delete(objects, key)
		}
	}))
	defer server.Close()
	projects, err := New(Config{Bucket: "kioku-projects", Endpoint: server.URL, Region: "auto", AccessKeyID: "test", SecretAccessKey: "secret", SessionToken: "session", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, pid := range []string{"alpha", "beta"} {
		if err := projects.EnsureProject(ctx, pid); err != nil {
			t.Fatal(err)
		}
		key := Key("projects/" + pid + "/original")
		if err := projects.Put(ctx, key, []byte(pid)); err != nil {
			t.Fatal(err)
		}
		b, err := projects.Get(ctx, key)
		if err != nil || string(b) != pid {
			t.Fatal("round trip", err)
		}
	}
	if projects.Bucket("alpha") != "kioku-projects" || projects.Bucket("beta") != "kioku-projects" {
		t.Fatal("unexpected bucket")
	}
	if err := projects.EnsureProject(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	keys, err := projects.List(ctx, "projects/alpha/")
	if err != nil || len(keys) != 1 || keys[0] != "projects/alpha/original" {
		t.Fatal(keys, err)
	}
	if err = projects.Delete(ctx, keys[0]); err != nil {
		t.Fatal(err)
	}
	if err = projects.DeleteProject(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	{
		projects.RegisterProjectBucket("legacy", "old-bucket")
		if err := projects.EnsureProject(ctx, "legacy"); err == nil {
			t.Fatal("silent migration allowed")
		}
		if _, err := projects.Get(ctx, "projects/legacy/original"); err == nil {
			t.Fatal("legacy bucket accessed")
		}
		if err := projects.DeleteProject(ctx, "legacy"); err == nil {
			t.Fatal("legacy deletion accepted")
		}
		if _, err := projects.List(ctx, "projects/"); err == nil {
			t.Fatal("unscoped list allowed")
		}
	}
	b, err := projects.Get(ctx, "projects/beta/original")
	if err != nil || string(b) != "beta" {
		t.Fatal("deletion affected another project", err)
	}
}
