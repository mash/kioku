package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mash/kioku/internal/runners"
)

func TestRateLimitOnlyTrustsConfiguredProxy(t *testing.T) {
	t.Parallel()
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
		{"ipv6", "fd00::/64", "[fd00::2]:1234", "2001:db8:0:1::1", "2001:db8:0:2::1", true},
		{"ipv6 same /64", "fd00::/64", "[fd00::2]:1234", "2001:db8::1", "2001:db8::2", false},
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

func TestRateLimitFullTableStillAdmitsNewHosts(t *testing.T) {
	t.Parallel()
	s := &Server{Config: Config{RatePerMinute: 1}}
	request := func(host string) bool {
		r := httptest.NewRequest("GET", "/api/projects", nil)
		r.RemoteAddr = host + ":1234"
		return s.allow(r)
	}
	for i := range maxRateHosts + 10 {
		if !request(net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)).String()) {
			t.Fatalf("host %d rejected while its own window is empty", i)
		}
	}
	if len(s.rates) > maxRateHosts {
		t.Fatalf("rate table grew past its bound: %d", len(s.rates))
	}
	if !request("192.0.2.1") || request("192.0.2.1") {
		t.Fatal("a full table must still admit a new host and still limit it")
	}
}

func TestRegisterRateLimitKeysIPv6ByPrefix(t *testing.T) {
	t.Parallel()
	e := newE2E(t, func(s *Server, _ *runners.Config) {
		_, s.Config.TrustedProxy, _ = net.ParseCIDR("127.0.0.0/8")
	})
	register := func(client string) int {
		body, _ := json.Marshal(map[string]any{"client_name": "Test connector", "redirect_uris": []string{e.url + "/callback"}})
		req, err := http.NewRequest("POST", e.url+"/oauth/register", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", client)
		return e.do(req).Code
	}
	for i := 1; i <= 10; i++ {
		if code := register(fmt.Sprintf("2001:db8:1:2::%x", i)); code != 201 {
			t.Fatalf("registration %d: HTTP %d", i, code)
		}
	}
	if code := register("2001:db8:1:2:ffff::1"); code != 429 {
		t.Fatalf("11th registration from one /64: HTTP %d, want 429", code)
	}
	if code := register("2001:db8:1:3::1"); code != 201 {
		t.Fatalf("registration from another /64: HTTP %d", code)
	}
}
