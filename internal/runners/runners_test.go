package runners

import (
	"errors"
	"testing"

	"github.com/mash/kioku/internal/runner"
)

func TestNewWithoutOperatorConfiguration(t *testing.T) {
	s, err := Load(Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.New()
	if r != nil || !errors.Is(err, runner.ErrNotConfigured) {
		t.Fatal(r, err)
	}
}

// E10: retired BYOK metadata is rejected or discarded by stopped migration,
// not decoded by runtime runner selection (covered in store migration tests).
