package server

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// SignupAllowlist restricts new accounts to Google identities from the listed
// hosted domains. Its zero value allows signup from any configured provider.
type SignupAllowlist struct {
	domains map[string]struct{}
}

type InvalidSignupDomain struct{ Entry string }

func (e InvalidSignupDomain) Error() string {
	return fmt.Sprintf("invalid KIOKU_SIGNUP_GOOGLE_HD entry %q", e.Entry)
}

var signupDomain = regexp.MustCompile(`^[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+$`)

func ParseSignupAllowlist(value string) (SignupAllowlist, error) {
	var allowlist SignupAllowlist
	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !signupDomain.MatchString(entry) {
			return SignupAllowlist{}, InvalidSignupDomain{Entry: entry}
		}
		if allowlist.domains == nil {
			allowlist.domains = make(map[string]struct{})
		}
		allowlist.domains[strings.ToLower(entry)] = struct{}{}
	}
	return allowlist, nil
}

func (a SignupAllowlist) allows(provider, hostedDomain string) bool {
	if len(a.domains) == 0 {
		return true
	}
	if provider != "google" || !signupDomain.MatchString(hostedDomain) {
		return false
	}
	_, ok := a.domains[strings.ToLower(hostedDomain)]
	return ok
}

var errSignupDenied = errors.New("signup denied")

const signupDeniedMessage = "このログイン ID では Kioku のアカウントを作れません。管理者に問い合わせてください。"
