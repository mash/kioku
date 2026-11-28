package source

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter reads only the first YAML block, delimited by complete --- lines.
// A closing delimiter at EOF is valid; empty blocks and duplicate keys are not.
func Frontmatter(content string) (*yaml.Node, error) {
	block, _, found := splitFrontmatter(content)
	if !found {
		return nil, errFrontmatter
	}
	return parseFrontmatter(block, false)
}

var errFrontmatter = errors.New("frontmatter missing or invalid: require a top YAML mapping with unique keys")

func parseFrontmatter(block string, rewriting bool) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(strings.NewReader(block))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, errFrontmatter
	}
	if rewriting {
		// Do not silently drop YAML that Decode left unread when rewriting.
		var extra yaml.Node
		if err := decoder.Decode(&extra); err != io.EOF {
			return nil, errFrontmatter
		}
	}
	mapping := root.Content[0]
	seen := make(map[string]bool)
	for i := 0; i < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
			return nil, errFrontmatter
		}
		seen[key.Value] = true
	}
	return mapping, nil
}

func HasFrontmatter(content string) bool {
	opening, _, _ := strings.Cut(content, "\n")
	return strings.TrimSuffix(opening, "\r") == "---"
}

func splitFrontmatter(content string) (string, string, bool) {
	_, rest, found := strings.Cut(content, "\n")
	if !found || !HasFrontmatter(content) {
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

// FrontmatterValue returns the node for key, or nil when key is absent.
func FrontmatterValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping != nil {
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == key {
				return mapping.Content[i+1]
			}
		}
	}
	return nil
}

// RewriteFrontmatter updates provenance in the top mapping without changing body bytes.
func RewriteFrontmatter(content, sourceURL, updatedAt string) (string, error) {
	block, body, found := splitFrontmatter(content)
	var mapping *yaml.Node
	if found {
		var err error
		mapping, err = parseFrontmatter(block, true)
		if err != nil {
			return "", err
		}
	} else if HasFrontmatter(content) {
		return "", errFrontmatter
	} else {
		mapping = &yaml.Node{Kind: yaml.MappingNode}
	}
	set := func(key, value string) {
		node := FrontmatterValue(mapping, key)
		if node == nil {
			node = &yaml.Node{}
			mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, node)
		}
		if node.Anchor != "" && node.Kind == yaml.ScalarNode && node.Value != value {
			preserveScalarAliases(mapping, node)
		}
		node.Kind, node.Tag, node.Value = yaml.ScalarNode, "!!str", value
		node.Alias, node.Content = nil, nil
	}
	if updatedAt != "" {
		set("updated_at", updatedAt)
	}
	set("source_url", sourceURL)
	encoded, err := yaml.Marshal(mapping)
	if err != nil {
		return "", fmt.Errorf("write frontmatter: %w", err)
	}
	return "---\n" + string(encoded) + "---\n" + body, nil
}

// Preserve other fields that refer to the old value of an edited scalar.
func preserveScalarAliases(root, old *yaml.Node) {
	if root.Kind == yaml.AliasNode && root.Alias == old {
		root.Kind, root.Style, root.Tag, root.Value = old.Kind, old.Style, old.Tag, old.Value
		root.Alias = nil
	}
	for _, child := range root.Content {
		preserveScalarAliases(child, old)
	}
}

// FrontmatterSourceURL reads the source identity from a parsed frontmatter mapping.
// File intake and Wiki apply their own identity rules.
func FrontmatterSourceURL(mapping *yaml.Node) (string, error) {
	value := FrontmatterValue(mapping, "source_url")
	if value != nil && value.Kind == yaml.ScalarNode && value.Tag == "!!str" && value.Value != "" {
		return value.Value, nil
	}
	return "", errors.New("source_url must be a nonempty string in YAML frontmatter")
}
