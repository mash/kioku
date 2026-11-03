package server

import (
	"net"
	"net/http/httptest"
	"testing"
)

func TestRateLimitOnlyTrustsConfiguredProxy(t *testing.T) {
	for _, tc := range []struct {
		name, cidr, remote, first, second string
		separate                          bool
	}{
		{"trusted separate clients", "172.30.83.0/24", "172.30.83.2:1234", "203.0.113.1", "203.0.113.2", true},
		{"untrusted forgery", "172.30.83.0/24", "198.51.100.3:1234", "203.0.113.1", "203.0.113.2", false},
		{"not configured", "", "172.30.83.2:1234", "203.0.113.1", "203.0.113.2", false},
		{"last value wins", "172.30.83.0/24", "172.30.83.2:1234", "1.1.1.1, 203.0.113.1", "8.8.8.8, 203.0.113.1", false},
		{"last clients differ", "172.30.83.0/24", "172.30.83.2:1234", "1.1.1.1, 203.0.113.1", "1.1.1.1, 203.0.113.2", true},
		{"invalid fallback", "172.30.83.0/24", "172.30.83.2:1234", "fake", "", false},
		{"ipv6", "fd00::/64", "[fd00::2]:1234", "2001:db8::1", "2001:db8::2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var network *net.IPNet
			if tc.cidr != "" {
				_, network, _ = net.ParseCIDR(tc.cidr)
			}
			s := &Server{Config: Config{RatePerMinute: 1, TrustedProxy: network}}
			r := httptest.NewRequest("GET", "/api/projects", nil)
			r.RemoteAddr = tc.remote
			r.Header.Set("X-Forwarded-For", tc.first)
			if !s.allow(r) {
				t.Fatal("first request rejected")
			}
			r.Header.Set("X-Forwarded-For", tc.second)
			if got := s.allow(r); got != tc.separate {
				t.Errorf("second allowed %v, want %v", got, tc.separate)
			}
		})
	}
}
