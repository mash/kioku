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

var (
	ErrLocalMarker     = errors.New("source_url: kioku:local is only for local documents")
	ErrLocalKeyForPut  = errors.New("local keys are only for remove; to put a local document use source_url: kioku:local")
	ErrHTTPSLocalFile  = errors.New("the local file has an HTTPS source_url")
	ErrLocalFileSource = errors.New("omit source_url or use kioku:local")
)

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

// LocalFileContent prepares a local file for upload without changing its body.
func LocalFileContent(name, content string, mtime time.Time) (string, error) {
	if _, err := File(name, content); err != nil {
		return "", err
	}
	block, body, found := splitFrontmatter(content)
	mapping := &yaml.Node{Kind: yaml.MappingNode}
	if found {
		decoder := yaml.NewDecoder(strings.NewReader(block))
		var err error
		mapping, err = decodeFrontmatter(decoder)
		if err != nil {
			return "", err
		}
		if FrontmatterValue(mapping, "source_url") != nil {
			u, _ := FrontmatterSourceURL(mapping)
			switch {
			case u == LocalSource:
			case strings.HasPrefix(strings.ToLower(u), "https:"):
				return "", ErrHTTPSLocalFile
			default:
				return "", ErrLocalFileSource
			}
		}
		if err := checkFrontmatterEnd(decoder); err != nil {
			return "", err
		}
	}
	return rewriteFrontmatter(mapping, body, LocalSource, mtime.UTC().Format(time.RFC3339Nano))
}

func fileWithProvenance(name, content string, allowLocal bool, localReceivedAt time.Time) (Document, string, error) {
	doc, err := File(name, content)
	if err != nil {
		return Document{}, "", err
	}
	block, body, found := splitFrontmatter(content)
	if !found {
		return Document{}, "", errFrontmatter
	}
	decoder := yaml.NewDecoder(strings.NewReader(block))
	metadata, err := decodeFrontmatter(decoder)
	if err != nil {
		return Document{}, "", err
	}
	sourceURL, err := FrontmatterSourceURL(metadata)
	if err != nil {
		return Document{}, "", err
	}
	isLocal := sourceURL == LocalSource
	if isLocal && !allowLocal {
		return Document{}, "", ErrLocalMarker
	}
	if IsLocalKey(sourceURL) {
		return Document{}, "", ErrLocalKeyForPut
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
		if strings.TrimSpace(body) == "" {
			return Document{}, "", errors.New("source body after frontmatter is empty")
		}
		if err := checkFrontmatterEnd(decoder); err != nil {
			return Document{}, "", err
		}
		sourceURL = fmt.Sprintf("%s/%x", LocalSource, sha256.Sum256([]byte(body)))
		content, err = rewriteFrontmatter(metadata, body, sourceURL, rewriteTime)
		if err != nil {
			return Document{}, "", err
		}
	}
	doc.URL = sourceURL
	return doc, content, nil
}
