package source

import (
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter reads only the first YAML block, delimited by complete --- lines.
// A closing delimiter at EOF is valid; empty blocks and duplicate keys are not.
func Frontmatter(content string) (*yaml.Node, error) {
	invalid := errors.New("frontmatter missing or invalid: require a top YAML mapping with unique keys")
	opening, rest, found := strings.Cut(content, "\n")
	if !found || strings.TrimSuffix(opening, "\r") != "---" {
		return nil, invalid
	}
	start := len(content) - len(rest)
	for offset := start; offset < len(content); {
		line, remaining, hasNewline := strings.Cut(content[offset:], "\n")
		if line == "---" || (hasNewline && line == "---\r") {
			block := strings.ReplaceAll(content[start:offset], "\r\n", "\n")
			var root yaml.Node
			if err := yaml.Unmarshal([]byte(block), &root); err != nil || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
				return nil, invalid
			}
			mapping := root.Content[0]
			seen := make(map[string]bool)
			for i := 0; i < len(mapping.Content); i += 2 {
				key := mapping.Content[i]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
					return nil, invalid
				}
				seen[key.Value] = true
			}
			return mapping, nil
		}
		if !hasNewline {
			break
		}
		offset = len(content) - len(remaining)
	}
	return nil, invalid
}

// FrontmatterSourceURL reads the source identity from a parsed frontmatter mapping.
// File intake also validates the URL; wiki output only requires a nonempty string.
func FrontmatterSourceURL(mapping *yaml.Node) (string, error) {
	for i := 0; i < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		if key.Value == "source_url" && value.Kind == yaml.ScalarNode && value.Tag == "!!str" && value.Value != "" {
			return value.Value, nil
		}
	}
	return "", errors.New("source_url must be a nonempty string in YAML frontmatter")
}
