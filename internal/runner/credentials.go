package runner

import (
	"errors"
	"strings"
)

// Credential is a header-only, host-limited vault secret.
type Credential struct {
	Name         string
	Value        Secret
	AllowedHosts []string
}

func VaultCredentials(spec RunSpec, host string) ([]Credential, error) {
	if spec.Token == "" {
		return nil, errors.New("invalid vault credential")
	}
	if host == "" || strings.ContainsAny(host, "*/\\@ \t\r\n") {
		return nil, errors.New("invalid credential destination")
	}
	return []Credential{{Name: "KIOKU_JOB_TOKEN", Value: spec.Token, AllowedHosts: []string{host}}}, nil
}
