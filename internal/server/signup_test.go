package server

import (
	"errors"
	"testing"
)

func TestParseSignupAllowlistRejectsInvalidDomains(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"*.example.com", "https://example.com", ".example.com", "example..com", "example.com.", "localhost", "a_b.example", "a b.example", "é.example", "K.example", "x\ny.example"} {
		a, err := ParseSignupAllowlist("valid.example, " + invalid + ", next_invalid")
		var problem InvalidSignupDomain
		if !errors.As(err, &problem) || problem.Entry != invalid || len(a.domains) != 0 {
			t.Errorf("did not reject first invalid entry %q: %v %v", invalid, a, err)
		}
	}
}
