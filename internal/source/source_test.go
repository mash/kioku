package source

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestSSRFGuards(t *testing.T) {
	for _, raw := range []string{"http://example.com", "https://localhost", "https://127.0.0.1", "https://[::1]", "https://169.254.169.254/latest/meta-data", "https://10.0.0.1", "https://user:pass@example.com", "https://example.com:8443"} {
		u, _ := url.Parse(raw)
		if ValidateURL(u) == nil {
			t.Errorf("allowed %s", raw)
		}
	}
	for _, ip := range []string{"127.0.0.1", "::ffff:127.0.0.1", "10.0.0.1", "100.64.0.1", "192.0.2.1", "2001:db8::1", "64:ff9b::a00:1"} {
		if publicIP(netip.MustParseAddr(ip)) {
			t.Errorf("allowed %s", ip)
		}
	}
	if !publicIP(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public IP rejected")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (fn roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }
func TestPublicHTMLWithoutExtension(t *testing.T) {
	f := New()
	f.Public = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credential sent to public host")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(`<html><title>Artifact</title><script>secret()</script><h1>Hello</h1><p>Shared context</p></html>`))}, nil
	})}
	d, err := f.Fetch(context.Background(), "https://example.com/artifacts/123", "must-not-leak")
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Artifact" || !strings.Contains(d.Text, "Shared context") || strings.Contains(d.Text, "secret()") {
		t.Fatalf("unexpected extraction: %+v", d)
	}
}

func TestUnavailableWebSourceRequiresFileInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"private source", http.StatusForbidden, "Forbidden"},
		{"JavaScript app shell", http.StatusOK, `<html><title>Artifact</title><body><div id="app"></div><script>document.querySelector('#app').textContent='Client-only text'</script></body></html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := New()
			f.Public = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			_, err := f.Fetch(context.Background(), "https://example.com/artifact", "")
			if err == nil || !strings.Contains(err.Error(), "kioku put --source-url <original-URL> <file>") {
				t.Fatalf("expected a file-put instruction, got %v", err)
			}
		})
	}
}
func TestGoogleTabOrder(t *testing.T) {
	raw := `{"tabs":[{"documentTab":{"body":{"content":[{"paragraph":{"elements":[{"textRun":{"content":"First\n"}}]}}]}},"childTabs":[{"documentTab":{"body":{"content":[{"paragraph":{"elements":[{"textRun":{"content":"Second\n"}}]}}]}}}]}]}`
	var d map[string]any
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	googleText(d, &b)
	if b.String() != "First\nSecond\n" {
		t.Fatal(b.String())
	}
}
