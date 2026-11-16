package runners

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mash/kioku/internal/runner"
)

func execConfig(t *testing.T) Config {
	t.Helper()
	return Config{Prompts: defaultPrompts(t), Type: "exec", Command: []string{os.Args[0]}, BaseURL: "http://127.0.0.1", DataDir: t.TempDir()}
}
func TestExecStartupValidation(t *testing.T) {
	for _, command := range [][]string{nil, {}, {""}, {"relative-command"}} {
		c := execConfig(t)
		c.Command = command
		_, err := Load(c, nil)
		if err == nil {
			t.Fatal(command)
		}
	}
	for _, origin := range []string{"", "not a URL", "http://user@host", "http://host/path", "http://host?q=x", "http://host#fragment", "//host", "file://host", "http://host?"} {
		c := execConfig(t)
		c.BaseURL = origin
		_, err := Load(c, nil)
		if err == nil {
			t.Fatal(origin)
		}
	}
	c := execConfig(t)
	c.AnthropicAPIKey = "partial"
	if _, err := Load(c, nil); err == nil {
		t.Fatal("exec bypassed claudemanaged configuration validation")
	}
	c = execConfig(t)
	if err := os.WriteFile(filepath.Join(c.DataDir, "exec-runs"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(c, nil); err == nil {
		t.Fatal("invalid root accepted")
	}
}
func TestExecStartupRootAndOtherRunners(t *testing.T) {
	c := execConfig(t)
	s, err := Load(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	info, err := os.Stat(filepath.Join(c.DataDir, "exec-runs"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal(info, err)
	}
	c = Config{DataDir: filepath.Join(t.TempDir(), "must-not-create")}
	s, err = Load(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	if _, err := os.Stat(c.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unconfigured runner created directories", err)
	}
}
func TestBaseOriginUsesManagedRule(t *testing.T) {
	for _, origin := range []string{"http://host", "https://host"} {
		t.Run(origin, func(t *testing.T) {
			c := execConfig(t)
			c.BaseURL = origin
			s, err := Load(c, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(t.Context())
			c.Type = "claudemanaged"
			c.MAAgentID, c.MAEnvironmentID, c.AnthropicAPIKey = "agent", "environment", "key"
			if _, err := Load(c, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCloseStopsExecRunner(t *testing.T) {
	c := execConfig(t)
	g, err := Load(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close(t.Context())
	r := g.Runner
	if err := g.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(t.Context(), runner.RunSpec{Key: runner.AttemptKey{JobID: "job"}}); err == nil {
		t.Fatal("closed runner accepted a new run")
	}
}

func TestExecStartupRequiresRunnableCommand(t *testing.T) {
	for _, kind := range []string{"missing", "nonexecutable", "directory"} {
		t.Run(kind, func(t *testing.T) {
			c := execConfig(t)
			path := filepath.Join(t.TempDir(), "command")
			switch kind {
			case "nonexecutable":
				if err := os.WriteFile(path, []byte("not executable"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			c.Command = []string{path}
			if _, err := Load(c, nil); err == nil || err.Error() != "KIOKU_RUNNER_COMMAND executable is not runnable" {
				t.Fatal("unrunnable command accepted or path leaked", err)
			}
			if _, err := os.Stat(filepath.Join(c.DataDir, "exec-runs")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid command created run root", err)
			}
			c.Type = ""
			s, err := Load(c, nil)
			if err != nil {
				t.Fatal("inactive exec command was validated", err)
			}
			defer s.Close(t.Context())
		})
	}
	c := execConfig(t)
	s, err := Load(c, nil)
	if err != nil {
		t.Fatal("runnable command rejected", err)
	}
	defer s.Close(t.Context())
}
