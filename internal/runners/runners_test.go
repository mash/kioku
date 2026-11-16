package runners

import (
	"testing"
)

func TestNewWithoutOperatorConfiguration(t *testing.T) {
	s, err := Load(Config{}, nil)
	if err != nil || s.Runner != nil {
		t.Fatal(s, err)
	}
}

// E10: retired BYOK metadata is rejected or discarded by stopped migration,
// not decoded by runtime runner selection (covered in store migration tests).
