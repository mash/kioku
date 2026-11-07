package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestBrowserOrigin(t *testing.T) {
	for _, tc := range []struct{ server, target, want string }{
		{"https://example.com", "https://EXAMPLE.COM:443/auth/start?device=one", "https://example.com"},
		{"https://EXAMPLE.COM:443/", "https://example.com/auth/start", "https://example.com"},
		{"https://example.com:8443", "https://EXAMPLE.COM:8443/auth/start", "https://example.com:8443"},
		{"http://localhost", "http://LOCALHOST:80/auth/start", "http://localhost"},
		{"http://127.0.0.1:80", "http://127.0.0.1/auth/start", "http://127.0.0.1"},
		{"http://[::1]:80", "http://[::1]/auth/start", "http://[::1]"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			origin, err := browserOrigin(tc.server, tc.target)
			if err != nil || origin != tc.want {
				t.Fatal(origin, err)
			}
			normalized, err := normalizeOrigin(tc.server)
			if err != nil || normalized != tc.want {
				t.Fatal(normalized, err)
			}
		})
	}
	// If validation regresses, an invalid URL must still never open a real app.
	t.Setenv("PATH", t.TempDir())
	for _, target := range []string{
		"https://evil.example/auth/start",
		"https://example.com:8443/auth/start",
		"http://example.com/auth/start",
		"file:///tmp/page.html",
		"kioku://login",
		"/auth/start",
		"-a Terminal",
		"//example.com/auth/start",
		"https://user@example.com/auth/start",
	} {
		t.Run(target, func(t *testing.T) {
			err := openBrowser("https://example.com", target)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", target)) || !strings.Contains(err.Error(), "server origin \"https://example.com\"") {
				t.Fatal("unsafe URL not rejected with the URL and the server origin", err)
			}
		})
	}
}

func TestLoginAndConnectRejectUnsafeBrowserURLs(t *testing.T) {
	for _, command := range []string{"login", "connect"} {
		for _, target := range []string{"https://evil.example/auth/start", "file:///tmp/page.html", "kioku://login", "/auth/start", "-a Terminal"} {
			t.Run(command+"/"+target, func(t *testing.T) {
				redirect := make(chan string, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if command == "login" {
						var req map[string]string
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Error(err)
						}
						redirect <- req["redirect_uri"]
					}
					w.WriteHeader(201)
					json.NewEncoder(w).Encode(map[string]string{"device": "test-device", "url": target})
				}))
				defer server.Close()
				var out bytes.Buffer
				a := &App{Config: Config{Server: server.URL}, Client: server.Client(), Out: &out, Err: &out, ctx: context.Background()}
				opened := false
				stubBrowser(t, func(_, _ string) error { opened = true; return nil })
				var err error
				if command == "login" {
					err = a.login(nil)
				} else {
					err = a.connect([]string{"google"})
				}
				if err == nil || !strings.Contains(err.Error(), server.URL) || opened || out.Len() != 0 {
					t.Fatal("unsafe browser URL accepted or displayed", err, opened, out.String())
				}
				if command == "login" {
					if response := fetchLoginPage(<-redirect); response.err == nil {
						t.Fatal("listener leaked on invalid origin")
					}
				}
			})
		}
	}
}

func TestLoginStartFailureClosesListener(t *testing.T) {
	for _, status := range []int{400, 429, 201} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			redirect := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]string
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				redirect <- req["redirect_uri"]
				w.WriteHeader(status)
				if status == 201 {
					json.NewEncoder(w).Encode(map[string]string{"url": "http://" + r.Host + "/auth/google/start"})
				} else {
					json.NewEncoder(w).Encode(map[string]string{"error": "start refused"})
				}
			}))
			defer server.Close()
			opened := false
			stubBrowser(t, func(_, _ string) error { opened = true; return nil })
			a := &App{Config: Config{Server: server.URL}, Client: server.Client(), Out: io.Discard, Err: io.Discard, ctx: context.Background()}
			want := "start refused"
			if status == 201 {
				want = "server returned an empty login request; run kioku login again"
			}
			if err := a.login(nil); err == nil || err.Error() != want || opened {
				t.Fatal(err, opened)
			}
			if response := fetchLoginPage(<-redirect); response.err == nil {
				t.Fatal("listener leaked after start failure")
			}
		})
	}
}

func TestConnectAndOpenUseValidatedBrowserURL(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/connect/google" {
			t.Error("unexpected path", r.URL.Path)
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"url": server.URL + "/auth/google/start?connect=one&ticket=two"})
	}))
	defer server.Close()
	var targets []string
	stubBrowser(t, func(origin, target string) error {
		if origin != server.URL {
			t.Error(origin)
		}
		targets = append(targets, target)
		return nil
	})
	a := &App{Config: Config{Server: server.URL, Namespace: "org/project", Project: "project-id"}, Client: server.Client(), Out: io.Discard, Err: io.Discard, ctx: context.Background()}
	if err := a.connect([]string{"google"}); err != nil {
		t.Fatal(err)
	}
	if err := a.open(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(targets, []string{server.URL + "/auth/google/start?connect=one&ticket=two", server.URL + "/org/project"}) {
		t.Fatal(targets)
	}
}
