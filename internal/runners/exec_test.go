package runners

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"github.com/mash/kioku/prompts"
)

func execConfig(t *testing.T) Config {
	t.Helper()
	return Config{Type: "exec", Command: []string{os.Args[0]}, BaseURL: "http://127.0.0.1", DataDir: t.TempDir()}
}
func TestExecStartupValidation(t *testing.T) {
	for _, command := range [][]string{nil, {}, {""}, {"relative-command"}} {
		c := execConfig(t)
		c.Command = command
		_, err := Load(c, nil, nil, nil)
		if err == nil {
			t.Fatal(command)
		}
	}
	for _, origin := range []string{"", "not a URL", "http://user@host", "http://host/path", "http://host?q=x", "http://host#fragment", "//host", "file://host", "http://host?"} {
		c := execConfig(t)
		c.BaseURL = origin
		_, err := Load(c, nil, nil, nil)
		if err == nil {
			t.Fatal(origin)
		}
	}
	c := execConfig(t)
	c.AnthropicAPIKey = "partial"
	if _, err := Load(c, nil, nil, nil); err == nil {
		t.Fatal("exec bypassed managed configuration validation")
	}
	c = execConfig(t)
	if err := os.WriteFile(filepath.Join(c.DataDir, "exec-runs"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(c, nil, nil, nil); err == nil {
		t.Fatal("invalid root accepted")
	}
	original := prompts.WikiDefinition
	defer func() { prompts.WikiDefinition = original }()
	for _, definition := range []string{"invalid", "---\nmodel: [unterminated\n---\nprompt", "---\nname: no-model\n---\nprompt", "---\nmodel: ''\n---\nprompt"} {
		prompts.WikiDefinition = definition
		if _, err := Load(execConfig(t), nil, nil, nil); err == nil {
			t.Fatal("invalid definition accepted")
		}
	}
}
func TestExecStartupRootAndReconcileOnly(t *testing.T) {
	c := execConfig(t)
	relative, err := filepath.Rel(".", c.DataDir)
	if err == nil {
		c.DataDir = relative
	}
	s, err := Load(c, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	if !filepath.IsAbs(s.config.DataDir) {
		t.Fatal(s.config.DataDir)
	}
	info, err := os.Stat(filepath.Join(s.config.DataDir, "exec-runs"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal(info, err)
	}
	c = Config{DataDir: filepath.Join(t.TempDir(), "must-not-create")}
	s, err = Load(c, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	if _, err := os.Stat(c.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reconcile-only runner created directories", err)
	}
	if s.execRunner == nil {
		t.Fatal("missing recovery runner")
	}
}
func TestBaseOriginUsesManagedRule(t *testing.T) {
	for _, origin := range []string{"http://host", "https://host"} {
		t.Run(origin, func(t *testing.T) {
			c := execConfig(t)
			c.BaseURL = origin
			s, err := Load(c, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(t.Context())
			c.Type = "managed"
			c.MAAgentID, c.MAEnvironmentID, c.AnthropicAPIKey = "agent", "environment", "key"
			if _, err := Load(c, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagedConfigurationCleansExecWorkdir(t *testing.T) {
	c := execConfig(t)
	s, err := Load(c, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := runner.AttemptKey{JobID: "job", AttemptID: "attempt"}
	a := runner.Attempt{Key: key}
	workdir := filepath.Join(c.DataDir, "exec-runs", key.JobID+"-"+key.AttemptID)
	if err := os.MkdirAll(workdir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "leftover"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.Type = "managed"
	c.MAAgentID, c.MAEnvironmentID, c.AnthropicAPIKey = "agent", "environment", "key"
	s, err = Load(c, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	if _, err := s.execRunner.Start(t.Context(), runner.RunSpec{}); err == nil {
		t.Fatal("recovery runner accepted Start")
	}
	if err := s.execRunner.Cleanup(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workdir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("leftover workdir remains", err)
	}
	if err := os.Remove(filepath.Dir(workdir)); err != nil {
		t.Fatal(err)
	}
	if err := s.execRunner.Cleanup(t.Context(), a); err != nil {
		t.Fatal("missing root was not treated as done", err)
	}
}

func TestRunnerOwnsCommandAndClose(t *testing.T) {
	c := execConfig(t)
	g, err := Load(c, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close(t.Context())
	c.Command[0] = "mutated-command"
	if g.config.Command[0] != os.Args[0] {
		t.Fatal("configuration aliases caller's command")
	}
	r, err := g.New(model.Job{})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(t.Context(), runner.RunSpec{Key: runner.AttemptKey{JobID: "job", AttemptID: "closed"}}); err == nil {
		t.Fatal("closed runner accepted a new run")
	}
}

func TestNewUsesCurrentRunnerType(t *testing.T) {
	c := execConfig(t)
	s, err := Load(c, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(t.Context())
	r, err := s.New(model.Job{Run: &model.AgentRun{Ref: runner.RunRef{ID: "previous-run"}}})
	if err != nil || r != s.execRunner {
		t.Fatal("current exec configuration was not selected", err)
	}
	s.config.Type = ""
	if _, err := s.New(model.Job{Run: &model.AgentRun{Ref: runner.RunRef{ID: "previous-run"}}}); !errors.Is(err, runner.ErrNotConfigured) {
		t.Fatal("saved run selected a runner without configuration", err)
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
			if _, err := Load(c, nil, nil, nil); err == nil || err.Error() != "KIOKU_RUNNER_COMMAND executable is not runnable" {
				t.Fatal("unrunnable command accepted or path leaked", err)
			}
			if _, err := os.Stat(filepath.Join(c.DataDir, "exec-runs")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid command created run root", err)
			}
			c.Type = ""
			s, err := Load(c, nil, nil, nil)
			if err != nil {
				t.Fatal("inactive exec command was validated", err)
			}
			defer s.Close(t.Context())
		})
	}
	c := execConfig(t)
	s, err := Load(c, nil, nil, nil)
	if err != nil {
		t.Fatal("runnable command rejected", err)
	}
	defer s.Close(t.Context())
}
