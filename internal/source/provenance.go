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
	metadata, err := Frontmatter(content)
	if err != nil {
		return Document{}, err
	}
	sourceURL, err := FrontmatterSourceURL(metadata)
	if err != nil {
		return Document{}, err
	}
	if err := ValidateSourceURL(sourceURL); err != nil {
		return Document{}, fmt.Errorf("source_url: %w", err)
	}
	var updatedAt yaml.Node
	for i := 0; i < len(metadata.Content); i += 2 {
		if metadata.Content[i].Value == "updated_at" {
			updatedAt = *metadata.Content[i+1]
		}
	}
	const invalidUpdatedAt = "updated_at must be an RFC 3339 timestamp with an explicit offset in YAML frontmatter"
	if updatedAt.Kind != yaml.ScalarNode || !provenanceTimestamp.MatchString(updatedAt.Value) {
		return Document{}, errors.New(invalidUpdatedAt)
	}
	if _, err := time.Parse(time.RFC3339, strings.ToUpper(updatedAt.Value)); err != nil {
		return Document{}, errors.New(invalidUpdatedAt)
	}
	doc.URL = sourceURL
	return doc, nil
}
