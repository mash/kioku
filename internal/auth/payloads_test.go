package auth

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestStoragePayloadsDoNotLogSecrets(t *testing.T) {
	for _, v := range []any{OAuthCookie{Verifier: "secret-verifier"}, JobCredential{JobID: "private-job"}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if got := fmt.Sprintf(format, v); got != "[redacted]" {
				t.Fatalf("payload leaked: %q", got)
			}
		}
		b, err := json.Marshal(v)
		if err != nil || string(b) != `"[redacted]"` {
			t.Fatalf("payload JSON: %s %v", b, err)
		}
	}
}
