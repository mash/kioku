package wiki

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/mash/kioku/internal/model"
	"gopkg.in/yaml.v3"
)

var outputPath = regexp.MustCompile(`^wiki/[a-z0-9][a-z0-9-]*\.md$`)

func ValidatePages(pages map[string]string) error {
	if _, ok := pages["wiki/index.md"]; !ok {
		return fmt.Errorf("wiki/index.md: required page missing")
	}
	for name := range pages {
		if !outputPath.MatchString(name) || !ValidFile(name) {
			return fmt.Errorf("%s: invalid wiki filename", name)
		}
	}
	return nil
}

func SourceURL(body string) (string, error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if !strings.HasPrefix(body, "---\n") {
		return "", fmt.Errorf("source_url frontmatter required")
	}
	rest := body[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 && strings.HasSuffix(rest, "\n---") {
		end = len(rest) - 4
	}
	if end < 0 {
		return "", fmt.Errorf("unterminated frontmatter")
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(rest[:end]), &root); err != nil {
		return "", fmt.Errorf("invalid frontmatter")
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("source_url frontmatter required")
	}
	var value string
	for i := 0; i < len(root.Content[0].Content); i += 2 {
		key, v := root.Content[0].Content[i], root.Content[0].Content[i+1]
		if key.Value == "source_url" {
			if value != "" || v.Kind != yaml.ScalarNode || v.Tag != "!!str" || v.Value == "" {
				return "", fmt.Errorf("source_url must be one nonempty string")
			}
			value = v.Value
		}
	}
	if value == "" {
		return "", fmt.Errorf("source_url frontmatter required")
	}
	return value, nil
}

// ValidateSources implements (a)-(e); source bodies and log entries are not checked.
func ValidateSources(pages, previous map[string]string, items []model.InboxItem) []string {
	var problems []string
	urls := map[string]string{}
	names := make([]string, 0, len(pages))
	for name := range pages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasPrefix(name, "wiki/source-") {
			continue
		}
		u, err := SourceURL(pages[name])
		if err != nil {
			problems = append(problems, "(a) "+name+": "+err.Error())
			continue
		}
		if prior, ok := urls[u]; ok {
			problems = append(problems, "(b) "+name+": duplicate source_url with "+prior)
		} else {
			urls[u] = name
		}
	}
	removed := map[string]bool{}
	for _, item := range items {
		if item.Status == "removed" {
			removed[item.SourceURL] = true
			if urls[item.SourceURL] != "" {
				problems = append(problems, "(c) removed source remains: "+item.SourceURL)
			}
		}
		if item.Status == "fetched" && urls[item.SourceURL] == "" {
			problems = append(problems, "(e) fetched source missing: "+item.SourceURL)
		}
	}
	for name, body := range previous {
		if !strings.HasPrefix(name, "wiki/source-") {
			continue
		}
		u, err := SourceURL(body)
		if err == nil && !removed[u] && urls[u] == "" {
			problems = append(problems, "(d) previous source missing: "+u)
		}
	}
	sort.Strings(problems)
	return problems
}
