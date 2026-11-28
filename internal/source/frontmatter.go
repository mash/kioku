package source

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter reads only the first YAML mapping delimited by complete --- lines.
// Empty blocks and closing delimiters at EOF are valid. Duplicate keys are not.
func Frontmatter(content string) (*yaml.Node, error) {
	fm, err := parseFrontmatter(content)
	if err != nil || fm.mapping == nil {
		return nil, errFrontmatter
	}
	return fm.mapping, nil
}

var errFrontmatter = errors.New("frontmatter missing or invalid: require a top YAML mapping with unique keys")

type markdownFrontmatter struct {
	mapping *yaml.Node
	body    string
	decoder *yaml.Decoder
}

func parseFrontmatter(content string) (markdownFrontmatter, error) {
	block, body, found := splitFrontmatter(content)
	absent := markdownFrontmatter{body: content}
	if !found {
		return absent, nil
	}
	if strings.TrimSpace(block) == "" {
		return markdownFrontmatter{mapping: &yaml.Node{Kind: yaml.MappingNode}, body: body}, nil
	}
	decoder := yaml.NewDecoder(strings.NewReader(block))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		// A pair of Markdown rules does not make the text between them metadata.
		return absent, nil
	}
	mapping := root.Content[0]
	seen := make(map[string]bool)
	for i := 0; i < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
			return markdownFrontmatter{}, errFrontmatter
		}
		seen[key.Value] = true
	}
	return markdownFrontmatter{mapping: mapping, body: body, decoder: decoder}, nil
}

func splitFrontmatter(content string) (string, string, bool) {
	opening, rest, found := strings.Cut(content, "\n")
	if !found || strings.TrimSuffix(opening, "\r") != "---" {
		return "", content, false
	}
	start := len(content) - len(rest)
	for offset := start; offset < len(content); {
		line, remaining, hasNewline := strings.Cut(content[offset:], "\n")
		if line == "---" || (hasNewline && line == "---\r") {
			return content[start:offset], remaining, true
		}
		if !hasNewline {
			break
		}
		offset = len(content) - len(remaining)
	}
	return "", content, false
}

// frontmatterValue returns the node for key, or nil when key is absent.
func frontmatterValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping != nil {
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == key {
				return mapping.Content[i+1]
			}
		}
	}
	return nil
}

// rewrite updates provenance without changing body bytes.
func (fm markdownFrontmatter) rewrite(sourceURL, updatedAt string) (string, error) {
	// Rewriting must not silently drop data after the first YAML document.
	if fm.decoder != nil && fm.decoder.Decode(new(yaml.Node)) != io.EOF {
		return "", errFrontmatter
	}
	mapping := fm.mapping
	if mapping == nil {
		mapping = &yaml.Node{Kind: yaml.MappingNode}
	}
	set := func(key, value string) error {
		next := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == key {
				if hasAnchorOrAlias(mapping.Content[i+1]) {
					return errFrontmatter
				}
				mapping.Content[i+1] = next
				return nil
			}
		}
		mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, next)
		return nil
	}
	if updatedAt != "" {
		if err := set("updated_at", updatedAt); err != nil {
			return "", err
		}
	}
	if err := set("source_url", sourceURL); err != nil {
		return "", err
	}
	encoded, err := yaml.Marshal(mapping)
	if err != nil {
		return "", fmt.Errorf("write frontmatter: %w", err)
	}
	return "---\n" + string(encoded) + "---\n" + fm.body, nil
}

func hasAnchorOrAlias(n *yaml.Node) bool {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return true
	}
	for _, child := range n.Content {
		if hasAnchorOrAlias(child) {
			return true
		}
	}
	return false
}

// FrontmatterSourceURL reads the source identity from a parsed frontmatter mapping.
// File intake and Wiki apply their own identity rules.
func FrontmatterSourceURL(mapping *yaml.Node) (string, error) {
	value := frontmatterValue(mapping, "source_url")
	if value != nil && value.Kind == yaml.ScalarNode && value.Tag == "!!str" && value.Value != "" {
		return value.Value, nil
	}
	return "", errors.New("source_url must be a nonempty string in YAML frontmatter")
}
