package source

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const LocalSource = "kioku:local"

var localKey = regexp.MustCompile(`^kioku:local/[0-9a-f]{64}$`)

func IsLocalKey(raw string) bool { return localKey.MatchString(raw) }

// ValidateSourceURL validates a stored source identity without fetching it.
func ValidateSourceURL(raw string) error {
	if IsLocalKey(raw) {
		return nil
	}
	if strings.HasPrefix(raw, LocalSource) {
		return errors.New("remove requires a full kioku:local/<64 lowercase hex> key")
	}
	return ValidateHTTPS(raw)
}

// ValidateHTTPS validates an HTTPS source identity, not a network destination.
func ValidateHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || strings.ContainsAny(raw, "\t\r\n") {
		return errors.New("invalid source URL")
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return errors.New("source URL must be HTTPS with a host and without user information")
	}
	return nil
}

// time.Parse accepts some non-RFC 3339 forms, including single-digit hours,
// comma fractions and out-of-range offsets. Check the syntax before parsing.
var provenanceTimestamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?([Zz]|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func ParseUpdatedAt(raw string) (time.Time, error) {
	if provenanceTimestamp.MatchString(raw) {
		if stamp, err := time.Parse(time.RFC3339, strings.ToUpper(raw)); err == nil {
			return stamp, nil
		}
	}
	return time.Time{}, errors.New("updated_at must be an RFC 3339 timestamp with an explicit offset in YAML frontmatter")
}

// FileWithProvenance reads the HTTPS provenance used by put PATH and capture put.
func FileWithProvenance(name, content string) (Document, error) {
	doc, _, err := fileWithProvenance(name, content, false, time.Time{})
	return doc, err
}

// InboxFile resolves the source identity and replaces the local key request in the inbox body.
func InboxFile(name, content string, localReceivedAt time.Time) (Document, string, error) {
	return fileWithProvenance(name, content, true, localReceivedAt)
}

func fileWithProvenance(name, content string, allowLocal bool, localReceivedAt time.Time) (Document, string, error) {
	doc, err := File(name, content)
	if err != nil {
		return Document{}, "", err
	}
	metadata, err := Frontmatter(content)
	if err != nil {
		return Document{}, "", err
	}
	sourceURL, err := FrontmatterSourceURL(metadata)
	if err != nil {
		return Document{}, "", err
	}
	isLocal := allowLocal && sourceURL == LocalSource
	if IsLocalKey(sourceURL) {
		return Document{}, "", errors.New("local keys are only for remove; to put a local document use source_url: kioku:local")
	}
	if !isLocal {
		if err := ValidateHTTPS(sourceURL); err != nil {
			return Document{}, "", fmt.Errorf("source_url: %w", err)
		}
	}
	var rewriteTime string
	if isLocal && !localReceivedAt.IsZero() {
		rewriteTime = localReceivedAt.UTC().Format(time.RFC3339Nano)
	} else {
		var raw string
		if value := FrontmatterValue(metadata, "updated_at"); value != nil && value.Kind == yaml.ScalarNode {
			raw = value.Value
		}
		if _, err := ParseUpdatedAt(raw); err != nil {
			return Document{}, "", err
		}
	}
	if isLocal {
		_, body, _ := splitFrontmatter(content)
		if strings.TrimSpace(body) == "" {
			return Document{}, "", errors.New("source body after frontmatter is empty")
		}
		sourceURL = fmt.Sprintf("%s/%x", LocalSource, sha256.Sum256([]byte(body)))
		content, err = RewriteFrontmatter(content, sourceURL, rewriteTime)
		if err != nil {
			return Document{}, "", err
		}
	}
	doc.URL = sourceURL
	return doc, content, nil
}
