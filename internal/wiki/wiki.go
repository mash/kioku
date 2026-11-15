package wiki

import (
	"path"
	"regexp"
)

var pagePath = regexp.MustCompile(`^wiki/[A-Za-z0-9][A-Za-z0-9_/-]*\.md$`)

func ValidFile(p string) bool {
	return p != "" && len(p) < 160 && path.Clean(p) == p && pagePath.MatchString(p)
}
