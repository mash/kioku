package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// Candidate discovery reads local hints only; the CLI still owns selection.
func sessionCandidates(prompt, cwd string) []projectEntry {
	var projects []projectEntry
	if home, err := os.UserHomeDir(); err == nil {
		a := &App{AuthDir: filepath.Join(home, ".config", "kioku", "auth")}
		if catalog, err := readProjects(a.projectsPath()); err == nil {
			projects = catalog.Projects
		}
	}
	if matches := matchingProjects(projects, func(p projectEntry) bool {
		return strings.Contains(prompt, p.Namespace)
	}); len(matches) != 0 {
		return matches
	}

	if !filepath.IsAbs(cwd) {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	return sessionConfigCandidates(cwd)
}

func sessionConfigCandidates(cwd string) []projectEntry {
	for dir := cwd; ; dir = filepath.Dir(dir) {
		a := &App{Dir: dir}
		if c, err := a.readProjectConfig(); err == nil && c.Namespace != "" {
			return []projectEntry{{Namespace: c.Namespace}}
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

func matchingProjects(projects []projectEntry, match func(projectEntry) bool) []projectEntry {
	var matches []projectEntry
	for _, p := range projects {
		if match(p) {
			matches = append(matches, p)
		}
	}
	return validUniqueProjects(matches)
}

func validUniqueProjects(projects []projectEntry) []projectEntry {
	var candidates []projectEntry
	seen := make(map[string]bool)
	for _, p := range projects {
		if !validNamespace(p.Namespace) {
			continue
		}
		key := p.Namespace
		if !seen[key] {
			candidates = append(candidates, projectEntry{Namespace: p.Namespace})
			seen[key] = true
		}
	}
	return candidates
}

func candidateContext(prompts string, candidates []projectEntry) string {
	if len(candidates) == 0 {
		return ""
	}
	if len(candidates) == 1 {
		prompt, _ := readHookPrompt(prompts, "hook-candidate.txt")
		return strings.ReplaceAll(prompt, "{{namespace}}", candidates[0].Namespace)
	}
	prompt, _ := readHookPrompt(prompts, "hook-candidates.txt")
	if prompt == "" {
		return ""
	}
	lines := []string{prompt}
	for _, p := range candidates {
		lines = append(lines, p.Namespace)
	}
	return strings.Join(lines, "\n")
}

func promptTurnContext(prompts string, s *sessionState) string {
	prompt, _ := readHookPrompt(prompts, "hook-turn.txt")
	line := strings.NewReplacer("{{namespace}}", s.Namespace, "{{wiki}}", filepath.Join(s.Path, "wiki")+string(filepath.Separator)).Replace(prompt)
	if s.LastPullResult == sessionPullFailure {
		line = joinHookContext(line, sessionPullFailedContext)
	}
	return line
}

func joinHookContext(first, second string) string {
	if first == "" {
		return second
	}
	if second == "" {
		return first
	}
	return first + "\n" + second
}
