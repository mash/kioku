package cli

import (
	"fmt"
	"strings"
)

func triggerKey(word string) string {
	for _, r := range word {
		if r > 127 {
			return word
		}
	}
	return strings.ToLower(word)
}

func normalizeTriggers(words []string) ([]string, error) {
	var normalized []string
	seen := make(map[string]bool)
	for _, word := range words {
		original := word
		word = strings.TrimSpace(word)
		if word == "" || strings.ContainsAny(original, "\t\r\n") {
			return nil, fmt.Errorf("project triggers: invalid word %q", original)
		}
		key := triggerKey(word)
		if !seen[key] {
			normalized = append(normalized, word)
			seen[key] = true
		}
	}
	return normalized, nil
}

func (a *App) projectTriggers(args []string) error {
	words, err := normalizeTriggers(args)
	if err != nil {
		return err
	}
	if _, err := a.projectPath(); err != nil {
		return err
	}
	unlock, err := a.localLock()
	if err != nil {
		return err
	}
	defer unlock()
	err = updateProjects(a.projectsPath(), func(p *projectsFile) bool {
		found := false
		for i := range p.Projects {
			if p.Projects[i].Server == a.Config.Server && p.Projects[i].Namespace == a.Config.Namespace {
				p.Projects[i].Triggers = words
				found = true
			}
		}
		if !found {
			p.Projects = append(p.Projects, projectEntry{Server: a.Config.Server, Namespace: a.Config.Namespace, Path: a.Dir, Triggers: words})
		}
		return true
	})
	if err != nil {
		return err
	}
	value := strings.Join(words, ", ")
	if value == "" {
		value = "（なし）"
	}
	_, err = fmt.Fprintf(a.Out, "%s triggers: %s\n", a.Config.Namespace, value)
	return err
}
