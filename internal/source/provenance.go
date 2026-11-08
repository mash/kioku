package source

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ValidateSourceURL validates provenance without fetching the original.
func ValidateSourceURL(raw string) error {
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

// FileWithProvenance reads provenance only from the submitted frontmatter.
func FileWithProvenance(name, content string) (Document, error) {
	doc, err := File(name, content)
	if err != nil {
		return Document{}, err
	}
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return Document{}, errors.New("source_url and updated_at are required in YAML frontmatter at the top of the content")
	}
	// Retain the opening newline so an immediately closing delimiter ends an
	// empty block instead of letting YAML consume metadata from the body.
	rest := normalized[3:]
	end := strings.Index(rest+"\n", "\n---\n")
	if end < 0 {
		return Document{}, errors.New("source_url and updated_at require a closed YAML frontmatter block")
	}
	var metadata struct {
		SourceURL yaml.Node `yaml:"source_url"`
		UpdatedAt yaml.Node `yaml:"updated_at"`
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &metadata); err != nil {
		return Document{}, fmt.Errorf("invalid source frontmatter (source_url and updated_at): %w", err)
	}
	if metadata.SourceURL.Kind != yaml.ScalarNode || metadata.SourceURL.Tag != "!!str" || metadata.SourceURL.Value == "" {
		return Document{}, errors.New("source_url must be a nonempty string in YAML frontmatter")
	}
	if err := ValidateSourceURL(metadata.SourceURL.Value); err != nil {
		return Document{}, fmt.Errorf("source_url: %w", err)
	}
	if metadata.UpdatedAt.Kind != yaml.ScalarNode || (metadata.UpdatedAt.Tag != "!!str" && metadata.UpdatedAt.Tag != "!!timestamp") || !provenanceTimestamp.MatchString(metadata.UpdatedAt.Value) {
		return Document{}, errors.New("updated_at must be a nonempty RFC 3339 timestamp with an explicit offset in YAML frontmatter")
	}
	if _, err := time.Parse(time.RFC3339, strings.ToUpper(metadata.UpdatedAt.Value)); err != nil {
		return Document{}, fmt.Errorf("updated_at must be an RFC 3339 timestamp with an explicit offset: %w", err)
	}
	doc.URL = metadata.SourceURL.Value
	return doc, nil
}
