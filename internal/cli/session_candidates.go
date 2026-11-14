package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

	// Resolve symlinks consistently with Git's reported working tree root.
	if filepath.IsAbs(cwd) {
		if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
			cwd = resolved
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		root := hookGit(ctx, cwd, "rev-parse", "--show-toplevel")
		if candidates := sessionConfigCandidates(cwd, root); len(candidates) != 0 {
			return candidates
		}
		if len(projects) == 0 {
			return nil
		}
		branch := hookGit(ctx, cwd, "symbolic-ref", "--quiet", "--short", "HEAD")
		return projectWordCandidates(projects, prompt, branch)
	}
	return projectWordCandidates(projects, prompt, "")
}

func hookGit(ctx context.Context, cwd string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func sessionConfigCandidates(cwd, root string) []projectEntry {
	// Without a Git root, inspect cwd itself, never unrelated ancestors.
	limit := cwd
	if filepath.IsAbs(root) {
		if rel, err := filepath.Rel(root, cwd); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			limit = root
		}
	}
	var candidates []projectEntry
	for dir := cwd; ; dir = filepath.Dir(dir) {
		a := &App{Dir: dir}
		if c, err := a.readProjectConfig(); err == nil && c.Namespace != "" {
			candidates = append(candidates, projectEntry{Server: c.Server, Namespace: c.Namespace, Path: dir})
		}
		if dir == limit || filepath.Dir(dir) == dir {
			return validUniqueProjects(candidates)
		}
	}
}

func projectWordCandidates(projects []projectEntry, prompt, branch string) []projectEntry {
	return matchingProjects(projects, func(p projectEntry) bool {
		_, project, _ := strings.Cut(p.Namespace, "/")
		return containsProjectWord(prompt, project) || containsProjectWord(branch, project)
	})
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
	seen := make(map[[2]string]bool)
	for _, p := range projects {
		if !validNamespace(p.Namespace) {
			continue
		}
		if _, err := normalizeOrigin(p.Server); err != nil {
			continue
		}
		key := [2]string{p.Server, p.Namespace}
		if !seen[key] {
			candidates = append(candidates, p)
			seen[key] = true
		}
	}
	return candidates
}

// ASCII word characters must not adjoin a project match. Japanese text and
// separators such as '/' and '-' can surround it without spaces.
func containsProjectWord(text, project string) bool {
	if project == "" {
		return false
	}
	for offset := 0; offset <= len(text)-len(project); {
		i := strings.Index(text[offset:], project)
		if i < 0 {
			return false
		}
		start := offset + i
		end := start + len(project)
		if (start == 0 || !asciiWord(text[start-1])) && (end == len(text) || !asciiWord(text[end])) {
			return true
		}
		offset = start + 1
	}
	return false
}

func asciiWord(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
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
	counts := make(map[string]int)
	for _, p := range candidates {
		counts[p.Namespace]++
	}
	lines := []string{prompt}
	for _, p := range candidates {
		line := p.Namespace
		if counts[p.Namespace] > 1 {
			line += "（server=" + p.Server + "）"
		}
		lines = append(lines, line)
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
