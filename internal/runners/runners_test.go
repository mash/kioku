package runners

import (
	"encoding/json"
	"errors"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/runner"
	"testing"
)

func TestNewWithoutOperatorConfiguration(t *testing.T) {
	s, err := Load(Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.New(model.Job{})
	if r != nil || !errors.Is(err, runner.ErrNotConfigured) {
		t.Fatal(r, err)
	}
}

func TestRetiredBYOKJobCannotUseServerRunner(t *testing.T) {
	var job model.Job
	if err := json.Unmarshal([]byte(`{"id":"old","anthropic_connection_id":"retired"}`), &job); err != nil {
		t.Fatal(err)
	}
	for _, selection := range []string{"", "claudemanaged", "exec", "openai"} {
		s := &Set{config: Config{Type: selection}}
		r, err := s.New(job)
		if r != nil || err == nil || errors.Is(err, runner.ErrNotConfigured) {
			t.Fatalf("%s: retired job must fail before selecting any runner: %v %v", selection, r, err)
		}
	}
}
