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

func TestVaultCredentialsContainsOnlyJobToken(t *testing.T) {
	spec := RunSpec{Token: "job-private"}
	credentials, err := VaultCredentials(spec, "kioku.test")
	if err != nil || len(credentials) != 1 {
		t.Fatal(credentials, err)
	}
	c := credentials[0]
	if c.Name != "KIOKU_JOB_TOKEN" || c.Value != spec.Token || len(c.AllowedHosts) != 1 || c.AllowedHosts[0] != "kioku.test" {
		t.Fatal(c)
	}
	b, _ := json.Marshal(spec)
	for _, rendered := range []string{string(b), fmt.Sprintf("%+v", spec), fmt.Sprintf("%#v", credentials)} {
		if strings.Contains(rendered, "job-private") {
			t.Fatal("secret leaked")
		}
	}
	for _, host := range []string{"", "*", "bad/host", "bad@host", "bad host"} {
		if _, err := VaultCredentials(spec, host); err == nil {
			t.Fatal("invalid destination accepted", host)
		}
	}
	if _, err := VaultCredentials(RunSpec{}, "kioku.test"); err == nil {
		t.Fatal("empty token accepted")
	}
}
