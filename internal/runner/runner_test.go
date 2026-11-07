package runner

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestSecret(t *testing.T) {
	s := Secret("secret-value")
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{s.String(), s.LogValue().String(), fmt.Sprintf("%v", s), string(b)} {
		if got != "[redacted]" && got != `"[redacted]"` {
			t.Fatalf("unexpected secret representation: %q", got)
		}
	}
}

func TestZeroStatusIsUnknown(t *testing.T) {
	if (Status{}).State != StateUnknown {
		t.Fatal("zero status must never prove a stop")
	}
}
