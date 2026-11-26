package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestKiokudSignupStartup(t *testing.T) {
	const child = "KIOKU_TEST_SIGNUP_STARTUP"
	if os.Getenv(child) == "1" {
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		os.Args = os.Args[:1]
		main()
		return
	}
	t.Parallel()
	for _, tc := range []struct{ name, allowlist, stderr string }{
		{"invalid", "nature.global,*.example.com,localhost", "kiokud: invalid KIOKU_SIGNUP_GOOGLE_HD entry \"*.example.com\"\n"},
		{"github-only", "nature.global", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKiokudSignupStartup$")
			dir := filepath.Join(t.TempDir(), "data")
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := listener.Addr().String()
			listener.Close()
			cmd.Env = []string{
				child + "=1", "GORACE=atexit_sleep_ms=0",
				"KIOKU_DATA=" + dir, "KIOKU_ADDR=" + addr, "KIOKU_BASE_URL=http://" + addr,
				"GITHUB_CLIENT_ID=test", "GITHUB_CLIENT_SECRET=test", "KIOKU_SIGNUP_GOOGLE_HD=" + tc.allowlist,
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if tc.stderr != "" {
				var exit *exec.ExitError
				if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 1 || stdout.Len() != 0 || stderr.String() != tc.stderr {
					t.Fatalf("startup = %v, stdout %q, stderr %q, want exit 1 and %q", err, &stdout, &stderr, tc.stderr)
				}
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("data directory was touched: %v", err)
				}
				return
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			client := &http.Client{Timeout: 100 * time.Millisecond}
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case err := <-done:
					t.Fatalf("server exited before readiness: %v\n%s", err, &stderr)
				case <-ctx.Done():
					<-done
					t.Fatalf("server not ready: %v\n%s", ctx.Err(), &stderr)
				case <-tick.C:
					resp, err := client.Get("http://" + addr + "/healthz")
					if err != nil {
						continue
					}
					resp.Body.Close()
					if resp.StatusCode == 200 {
						_ = cmd.Process.Signal(os.Interrupt)
						if err := <-done; err != nil {
							t.Fatalf("server shutdown: %v\n%s", err, &stderr)
						}
						return
					}
				}
			}
		})
	}
}
