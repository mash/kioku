package server

import (
	"net/http"
	"strings"

	"golang.org/x/mod/semver"
)

// MinimumClientVersion is the first release of the inbox API and project CLI.
const MinimumClientVersion = "0.1.0"

func checkClientVersion(r *http.Request) error {
	if !strings.HasPrefix(r.URL.Path, "/api/") || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return nil
	}
	v := r.Header.Get("Kioku-Client-Version")
	if v == "dev" {
		return nil
	}
	v = "v" + strings.TrimPrefix(v, "v")
	if !semver.IsValid(v) || semver.Compare(v, "v"+MinimumClientVersion) < 0 {
		return fail(426, "upgrade kioku to "+MinimumClientVersion+" or later")
	}
	return nil
}
