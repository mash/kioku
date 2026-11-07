package source

import (
	"net/netip"
	"net/url"
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
