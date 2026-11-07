package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func testStore(t *testing.T, handler http.HandlerFunc) *Projects {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	store, err := New(Config{Endpoint: server.URL, Region: "eu-west-1", Bucket: "kioku-projects", AccessKeyID: "test", SecretAccessKey: "secret", SessionToken: "session", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestSDKErrorClassificationAndRedirects(t *testing.T) {
	var redirected atomic.Int32
	store := testStore(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kioku-projects/projects/p/missing":
			w.WriteHeader(404)
		case "/kioku-projects/projects/p/denied":
			w.WriteHeader(403)
		case "/kioku-projects/projects/p/redirect":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "/redirected":
			redirected.Add(1)
			w.WriteHeader(200)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(400)
		}
	})
	for _, name := range []string{"missing", "denied", "redirect"} {
		_, err := store.Get(context.Background(), Key("projects/p/"+name))
		if err == nil {
			t.Fatalf("%s unexpectedly succeeded", name)
		}
		var missing interface{ NotFound() bool }
		isMissing := errors.As(err, &missing) && missing.NotFound()
		if isMissing != (name == "missing") {
			t.Fatalf("incorrect missing classification for %s: %v", name, err)
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("followed redirect")
	}
}

func TestSDKPagination(t *testing.T) {
	for _, badToken := range []bool{false, true} {
		t.Run(fmt.Sprint(badToken), func(t *testing.T) {
			var calls atomic.Int32
			store := testStore(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Query().Get("list-type") != "2" || r.URL.Query().Get("prefix") != "projects/p/" {
					t.Error("incorrect list request")
				}
				w.Header().Set("Content-Type", "application/xml")
				if r.URL.Query().Get("continuation-token") == "" {
					fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken><Contents><Key>projects/p/first</Key></Contents></ListBucketResult>`)
				} else if badToken {
					fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken></ListBucketResult>`)
				} else {
					fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>projects/p/second</Key></Contents></ListBucketResult>`)
				}
			})
			keys, err := store.List(context.Background(), "projects/p/")
			if badToken {
				if err == nil {
					t.Fatal("invalid pagination returned partial success")
				}
			} else if err != nil || len(keys) != 2 || keys[0] != "projects/p/first" || keys[1] != "projects/p/second" {
				t.Fatalf("pagination: %v %v", keys, err)
			}
			if calls.Load() != 2 {
				t.Fatalf("unexpected number of pages: %d", calls.Load())
			}
		})
	}
}
