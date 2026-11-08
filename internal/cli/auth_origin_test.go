package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthenticationRequiresTerminalApprovalRegardlessOfServerSelection(t *testing.T) {
	for _, selection := range []string{"project", "flag", "environment"} {
		for _, command := range [][]string{{"login"}, {"agent", "login", "kioku_code.secret"}} {
			t.Run(selection+"/"+strings.Join(command, " "), func(t *testing.T) {
				f := newOrderFixture(t)
				f.bind()
				previous := confirmAuthenticationServer
				confirmAuthenticationServer = func(out io.Writer, origin string) error {
					return promptAuthenticationServer(strings.NewReader("yes\n"), out, origin, false)
				}
				t.Cleanup(func() { confirmAuthenticationServer = previous })
				f.writeHuman(orderHumanToken)
				// Reject before reading credentials, making requests, or opening the browser.
				if err := os.WriteFile(f.app.credentialPath(), []byte("invalid credentials JSON"), 0600); err != nil {
					t.Fatal(err)
				}
				args := command
				switch selection {
				case "project":
					t.Setenv("KIOKU_SERVER", "")
				case "flag":
					t.Setenv("KIOKU_SERVER", "")
					args = append([]string{"--server", f.server.URL}, command...)
				}
				opened := false
				stubBrowser(t, func(_, _ string) error { opened = true; return nil })
				err := f.run(args...)
				if err == nil || !strings.Contains(err.Error(), "run this command in a terminal") || !strings.Contains(f.err.String(), f.server.URL) {
					t.Fatal("untrusted authentication server accepted", err)
				}
				if len(f.server.paths) != 0 || opened {
					t.Fatal("untrusted server contacted or browser opened", f.server.paths)
				}
				if strings.Contains(err.Error(), "kioku_code.secret") {
					t.Fatal("login code leaked in error")
				}
			})
		}
	}
}

func TestAuthenticationKeepsServerConflictGuard(t *testing.T) {
	for _, command := range [][]string{{"login"}, {"agent", "login", "kioku_code.secret"}} {
		f := newOrderFixture(t)
		f.bind()
		err := f.run(append([]string{"--server", defaultServer}, command...)...)
		if err == nil || !strings.Contains(err.Error(), "server conflicts") {
			t.Fatal(err)
		}
		if len(f.server.paths) != 0 {
			t.Fatal("conflicting server contacted")
		}
	}
}

func TestDefaultAuthenticationNeedsNoExplicitServer(t *testing.T) {
	for _, bound := range []bool{false, true} {
		f := newOrderFixture(t)
		t.Setenv("KIOKU_SERVER", "")
		if bound {
			f.app.Config.Server = defaultServer
			f.bind()
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var out bytes.Buffer
		// A canceled context guarantees no external request, while still running
		// origin selection and reaching the login request for the built-in server.
		err := Run(ctx, []string{"-C", f.dir, "login"}, &out, &out)
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatal("default server rejected before login", bound, err)
		}
	}
}

func TestProjectReadStillUsesConfiguredServer(t *testing.T) {
	f := newOrderFixture(t)
	f.bind()
	f.writeHuman(orderHumanToken)
	t.Setenv("KIOKU_SERVER", "")
	if err := f.run("whoami"); err != nil {
		t.Fatal(err)
	}
	if len(f.server.paths) != 1 || f.server.auths[0] != "Bearer "+orderHumanToken {
		t.Fatal("normal project credential selection changed", f.server.paths)
	}
}

func TestAuthenticationRemembersApproval(t *testing.T) {
	f := newOrderFixture(t)
	f.bind()
	t.Setenv("KIOKU_SERVER", "")
	previous := confirmAuthenticationServer
	calls := 0
	confirmAuthenticationServer = func(out io.Writer, origin string) error {
		calls++
		if len(f.server.paths) != 0 {
			t.Fatal("contacted server before approval")
		}
		return promptAuthenticationServer(strings.NewReader("yes\n"), out, origin, true)
	}
	t.Cleanup(func() { confirmAuthenticationServer = previous })
	if err := f.run("--server", f.server.URL, "agent", "login", "kioku_code.secret"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.err.String(), f.server.URL) {
		t.Fatal("origin not displayed")
	}
	if err := f.run("agent", "login", "kioku_code.secret"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(f.server.paths) != 2 {
		t.Fatal("approval not reused", calls, f.server.paths)
	}
	info, err := os.Stat(f.app.credentialPath() + ".trusted")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("trust record permissions", err)
	}
	// Trust is scoped to the origin, not the project or its directory.
	f.app.Config.Server = "https://other.example"
	f.app.Err = &f.err
	confirmAuthenticationServer = func(out io.Writer, origin string) error {
		return promptAuthenticationServer(strings.NewReader("no\n"), out, origin, true)
	}
	if err := f.app.trustAuthenticationServer(); err == nil {
		t.Fatal("different origin trusted")
	}
}

func TestAuthenticationDeclineDoesNotPersistOrConnect(t *testing.T) {
	for _, answer := range []string{"no\n", "\n", "yes"} {
		t.Run(answer, func(t *testing.T) {
			f := newOrderFixture(t)
			f.bind()
			t.Setenv("KIOKU_SERVER", "")
			previous := confirmAuthenticationServer
			confirmAuthenticationServer = func(out io.Writer, origin string) error {
				return promptAuthenticationServer(strings.NewReader(answer), out, origin, true)
			}
			t.Cleanup(func() { confirmAuthenticationServer = previous })
			if err := f.run("agent", "login", "kioku_code.secret"); err == nil {
				t.Fatal("decline accepted")
			}
			if len(f.server.paths) != 0 {
				t.Fatal("server contacted")
			}
			if _, err := os.Stat(f.app.credentialPath() + ".trusted"); !os.IsNotExist(err) {
				t.Fatal("trust persisted", err)
			}
		})
	}
}

func TestAuthenticationRejectsInvalidTrustRecord(t *testing.T) {
	for _, content := range []string{`"https://other.example"`, `{broken`} {
		f := newOrderFixture(t)
		f.bind()
		t.Setenv("KIOKU_SERVER", "")
		if err := os.MkdirAll(f.app.AuthDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.app.credentialPath()+".trusted", []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := f.run("login"); err == nil {
			t.Fatal("bad trust record accepted")
		}
		if len(f.server.paths) != 0 {
			t.Fatal("server contacted")
		}
	}
}

func TestAuthenticationRejectsSymlinkTrustRecord(t *testing.T) {
	f := newOrderFixture(t)
	f.bind()
	t.Setenv("KIOKU_SERVER", "")
	if err := os.MkdirAll(f.app.AuthDir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "trust")
	if err := os.WriteFile(target, []byte(`"`+f.server.URL+`"`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.app.credentialPath()+".trusted"); err != nil {
		t.Fatal(err)
	}
	if err := f.run("login"); err == nil {
		t.Fatal("symlink trusted")
	}
	if len(f.server.paths) != 0 {
		t.Fatal("server contacted")
	}
}
