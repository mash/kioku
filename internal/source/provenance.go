package source

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidateSourceURL validates provenance without fetching the original.
func ValidateSourceURL(raw string) error {
	if raw == "" {
		return errors.New("original source URL is required: use source_url frontmatter or --source-url")
	}
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 {
		return errors.New("invalid source URL")
	}
	return ValidateURL(u)
}

// FileWithURL extracts text while retaining a caller-supplied original URL.
// Explicit metadata takes precedence over source_url in YAML frontmatter.
func FileWithURL(name, content, sourceURL string) (Document, error) {
	doc, err := File(name, content)
	if err != nil {
		return Document{}, err
	}
	if sourceURL == "" {
		normalized := strings.ReplaceAll(content, "\r\n", "\n")
		if strings.HasPrefix(normalized, "---\n") {
			rest := normalized[4:]
			if end := strings.Index(rest, "\n---\n"); end >= 0 {
				var metadata struct {
					SourceURL string `yaml:"source_url"`
				}
				if err := yaml.Unmarshal([]byte(rest[:end]), &metadata); err != nil {
					return Document{}, fmt.Errorf("invalid source frontmatter: %w", err)
				}
				sourceURL = metadata.SourceURL
			}
		}
	}
	if err := ValidateSourceURL(sourceURL); err != nil {
		return Document{}, err
	}
	doc.URL = sourceURL
	return doc, nil
}
