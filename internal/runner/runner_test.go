package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSecret(t *testing.T) {
	s := Secret("secret-value")
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{s.String(), s.LogValue().String(), fmt.Sprintf("%v", s), fmt.Sprintf("%#v", s), string(b)} {
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

func TestCredentialRedactionAndDestinationValidation(t *testing.T) {
	credential := Credential{Name: "GOOGLE_ACCESS_TOKEN", Value: "private-value", AllowedHosts: []string{"docs.googleapis.com"}}
	spec := RunSpec{Token: "job-private", SourceCredentials: []Credential{credential}}
	b, _ := json.Marshal(spec)
	for _, rendered := range []string{string(b), fmt.Sprintf("%+v", spec), fmt.Sprintf("%#v", spec)} {
		if strings.Contains(rendered, "private-value") || strings.Contains(rendered, "job-private") {
			t.Fatal("secret leaked")
		}
	}
	for _, c := range []Credential{{Name: "KIOKU_JOB_TOKEN", Value: "x", AllowedHosts: []string{"evil.test"}}, {Name: "BAD-NAME", Value: "x", AllowedHosts: []string{"example.com"}}, {Name: "TOKEN", Value: "x"}, {Name: "TOKEN", Value: "x", AllowedHosts: []string{"*"}}} {
		spec.SourceCredentials = []Credential{c}
		if _, err := VaultCredentials(spec, "kioku.test"); err == nil {
			t.Fatal("invalid destination accepted")
		}
	}
}
