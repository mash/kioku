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

// LocalKeyLength is the byte length of a full local key.
const LocalKeyLength = len(LocalSource+"/YYYYMMDD-") + 2*sha256.Size

const invalidLocalKey = "invalid kioku:local key: use kioku:local or kioku:local/YYYYMMDD-<64 lowercase hex>"

var localKey = regexp.MustCompile(`^kioku:local/([0-9]{8})-[0-9a-f]{64}$`)

// LocalKeyDate returns the date of a full local key, or an empty string.
func LocalKeyDate(raw string) string {
	m := localKey.FindStringSubmatch(raw)
	if m == nil {
		return ""
	}
	if _, err := time.Parse("20060102", m[1]); err != nil {
		return ""
	}
	return m[1]
}

// ValidateSourceURL validates a stored source identity without fetching it.
func ValidateSourceURL(raw string) error {
	switch {
	case raw == LocalSource:
		return errors.New("remove requires a full kioku:local/YYYYMMDD-<hash> key")
	case LocalKeyDate(raw) != "":
		return nil
	case strings.HasPrefix(raw, LocalSource):
		return errors.New(invalidLocalKey)
	}
	return ValidateHTTPS(raw)
}

// ValidateHTTPS validates a public source URL without fetching it.
func ValidateHTTPS(raw string) error {
	if raw == "" {
		return errors.New("source_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 {
		return errors.New("invalid source URL")
	}
	return ValidateURL(u)
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

// FileWithProvenance reads the public HTTPS provenance used by put PATH and capture put.
func FileWithProvenance(name, content string) (Document, error) {
	doc, _, err := fileWithProvenance(name, content, false)
	return doc, err
}

// InboxFile resolves the source identity and replaces the local key request in the inbox body.
func InboxFile(name, content string) (Document, string, error) {
	return fileWithProvenance(name, content, true)
}

func fileWithProvenance(name, content string, allowLocal bool) (Document, string, error) {
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
	isLocal := allowLocal && strings.HasPrefix(sourceURL, LocalSource)
	switch {
	case isLocal && sourceURL != LocalSource && LocalKeyDate(sourceURL) == "":
		return Document{}, "", errors.New(invalidLocalKey)
	case !isLocal:
		if err := ValidateHTTPS(sourceURL); err != nil {
			return Document{}, "", fmt.Errorf("source_url: %w", err)
		}
	}
	updatedAt := FrontmatterValue(metadata, "updated_at")
	var raw string
	if updatedAt != nil && updatedAt.Kind == yaml.ScalarNode {
		raw = updatedAt.Value
	}
	stamp, err := ParseUpdatedAt(raw)
	if err != nil {
		if sourceURL == LocalSource && isLocal {
			err = errors.New("updated_at is required for source_url kioku:local: write the document's own date (for example the meeting date) as an RFC 3339 timestamp with an explicit offset")
		}
		return Document{}, "", err
	}
	if isLocal {
		_, body, _ := splitFrontmatter(content)
		if strings.TrimSpace(body) == "" {
			return Document{}, "", errors.New("source body after frontmatter is empty")
		}
		date := stamp.In(time.FixedZone("JST", 9*60*60)).Format("20060102")
		key := fmt.Sprintf("%s/%s-%x", LocalSource, date, sha256.Sum256([]byte(body)))
		if LocalKeyDate(key) == "" {
			return Document{}, "", errors.New("updated_at does not have a four-digit JST year")
		}
		if sourceURL == LocalSource {
			content, err = RewriteFrontmatter(content, key, "")
			if err != nil {
				return Document{}, "", err
			}
			sourceURL = key
		} else if sourceURL != key {
			return Document{}, "", fmt.Errorf("source_url %s does not match the body and updated_at; send source_url: kioku:local to get the current key", sourceURL)
		}
	}
	doc.URL = sourceURL
	return doc, content, nil
}
