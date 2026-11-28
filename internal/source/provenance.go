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

func parseUpdatedAt(raw string) (time.Time, error) {
	if provenanceTimestamp.MatchString(raw) {
		if stamp, err := time.Parse(time.RFC3339, strings.ToUpper(raw)); err == nil {
			return stamp, nil
		}
	}
	return time.Time{}, errors.New("updated_at must be an RFC 3339 timestamp with an explicit offset in YAML frontmatter")
}

// FileWithProvenance reads the HTTPS provenance used by put PATH and capture put.
func FileWithProvenance(name, content string) (Document, error) {
	doc, fm, err := fileWithProvenance(name, content)
	if err != nil {
		return Document{}, err
	}
	if doc.URL == LocalSource {
		return Document{}, ErrLocalMarker
	}
	if err := fm.validateUpdatedAt(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// InboxFile resolves the source identity and replaces the local key request in the inbox body.
func InboxFile(name, content string, localReceivedAt time.Time) (Document, string, error) {
	doc, fm, err := fileWithProvenance(name, content)
	if err != nil {
		return Document{}, "", err
	}
	isLocal := doc.URL == LocalSource
	var rewriteTime string
	if isLocal && !localReceivedAt.IsZero() {
		rewriteTime = localReceivedAt.UTC().Format(time.RFC3339Nano)
	} else if err := fm.validateUpdatedAt(); err != nil {
		return Document{}, "", err
	}
	if !isLocal {
		return doc, content, nil
	}
	if strings.TrimSpace(fm.body) == "" {
		return Document{}, "", errors.New("source body after frontmatter is empty")
	}
	doc.URL = fmt.Sprintf("%s/%x", LocalSource, sha256.Sum256([]byte(fm.body)))
	content, err = fm.rewrite(doc.URL, rewriteTime)
	if err != nil {
		return Document{}, "", err
	}
	return doc, content, nil
}

// LocalFileContent prepares a local file for upload without changing its body.
func LocalFileContent(name, content string, mtime time.Time) (string, error) {
	_, fm, err := parseFileFrontmatter(name, content)
	if err != nil {
		return "", err
	}
	if value := frontmatterValue(fm.mapping, "source_url"); value != nil {
		if hasAnchorOrAlias(value) {
			return "", errFrontmatter
		}
		u, _ := FrontmatterSourceURL(fm.mapping)
		switch {
		case u == LocalSource:
		case strings.HasPrefix(strings.ToLower(u), "https:"):
			return "", ErrHTTPSLocalFile
		default:
			return "", ErrLocalFileSource
		}
	}
	return fm.rewrite(LocalSource, mtime.UTC().Format(time.RFC3339Nano))
}

func parseFileFrontmatter(name, content string) (Document, markdownFrontmatter, error) {
	doc, err := File(name, content)
	if err != nil {
		return Document{}, markdownFrontmatter{}, err
	}
	fm, err := parseFrontmatter(content)
	return doc, fm, err
}

func fileWithProvenance(name, content string) (Document, markdownFrontmatter, error) {
	doc, fm, err := parseFileFrontmatter(name, content)
	if err != nil {
		return Document{}, fm, err
	}
	if fm.mapping == nil {
		return Document{}, fm, errFrontmatter
	}
	if value := frontmatterValue(fm.mapping, "source_url"); value != nil && value.Kind != yaml.ScalarNode && hasAnchorOrAlias(value) {
		return Document{}, fm, errFrontmatter
	}
	doc.URL, err = FrontmatterSourceURL(fm.mapping)
	if err != nil {
		return Document{}, fm, err
	}
	if IsLocalKey(doc.URL) {
		return Document{}, fm, ErrLocalKeyForPut
	}
	if doc.URL != LocalSource {
		if err := ValidateHTTPS(doc.URL); err != nil {
			return Document{}, fm, fmt.Errorf("source_url: %w", err)
		}
	}
	return doc, fm, nil
}

func (fm markdownFrontmatter) validateUpdatedAt() error {
	var raw string
	if value := frontmatterValue(fm.mapping, "updated_at"); value != nil && value.Kind == yaml.ScalarNode {
		raw = value.Value
	}
	_, err := parseUpdatedAt(raw)
	return err
}
