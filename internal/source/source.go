// Package source validates source provenance without fetching it.
package source

import (
	"errors"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

type Document struct {
	URL       string
	UpdatedAt time.Time
}

func File(name, content string) (Document, error) {
	if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return Document{}, errors.New("source must be UTF-8 text")
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".txt", ".html", ".htm", ".json", ".go", ".js", ".ts", ".tsx", ".jsx", ".yaml", ".yml", ".csv", ".css":
	default:
		return Document{}, errors.New("unsupported file extension; use text, Markdown, HTML or code")
	}
	if strings.TrimSpace(content) == "" {
		return Document{}, errors.New("source is empty")
	}
	return Document{}, nil
}
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, s := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001::/32", "2002::/16", "64:ff9b::/96", "64:ff9b:1::/48"} {
		if netip.MustParsePrefix(s).Contains(ip) {
			return false
		}
	}
	return true
}
func ValidateURL(u *url.URL) error {
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return errors.New("only public HTTPS URLs on port 443 are supported")
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return errors.New("local hostname blocked")
	}
	if ip, err := netip.ParseAddr(h); err == nil && !publicIP(ip) {
		return errors.New("private address blocked")
	}
	return nil
}
