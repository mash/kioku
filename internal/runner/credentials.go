package runner

import (
	"errors"
	"regexp"
	"strings"
)

// Credential is a header-only, host-limited vault secret. Repositories are
// nonsecret hints for selecting among installation tokens for the same host.
type Credential struct {
	Name         string
	Value        Secret
	AllowedHosts []string
	Repositories []string
}

var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func VaultCredentials(spec RunSpec, host string) ([]Credential, error) {
	credentials := append([]Credential{{Name: "KIOKU_JOB_TOKEN", Value: spec.Token, AllowedHosts: []string{host}}}, spec.SourceCredentials...)
	names := map[string]bool{}
	for _, c := range credentials {
		if !secretName.MatchString(c.Name) || names[c.Name] || c.Value == "" || len(c.AllowedHosts) == 0 {
			return nil, errors.New("invalid vault credential")
		}
		names[c.Name] = true
		for _, h := range c.AllowedHosts {
			if h == "" || strings.ContainsAny(h, "*/\\@ \t\r\n") {
				return nil, errors.New("invalid credential destination")
			}
		}
	}
	return credentials, nil
}
