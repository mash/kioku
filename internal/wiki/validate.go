package wiki

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
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
	metadata, err := source.Frontmatter(body)
	if err != nil {
		return "", err
	}
	return source.FrontmatterSourceURL(metadata)
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
		if item.Remove {
			removed[item.SourceURL] = true
			if urls[item.SourceURL] != "" {
				problems = append(problems, "(c) removed source remains: "+item.SourceURL)
			}
		}
		if !item.Remove && urls[item.SourceURL] == "" {
			problems = append(problems, "(e) submitted source missing: "+item.SourceURL)
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
