package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// walkAncestors reads the nearest .git and valid .kioku/config in one traversal.
func walkAncestors(cwd string) (branch string, config Config) {
	if !filepath.IsAbs(cwd) {
		return "", Config{}
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	foundGit := false
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if config.Namespace == "" {
			if c, err := (&App{Dir: dir}).readProjectConfig(); err == nil {
				config = c
			}
		}
		if !foundGit {
			git := filepath.Join(dir, ".git")
			if info, err := os.Stat(git); err == nil {
				foundGit = true
				if !info.IsDir() {
					data, err := os.ReadFile(git)
					if err == nil && strings.HasPrefix(string(data), "gitdir:") {
						git = strings.TrimSpace(strings.TrimPrefix(string(data), "gitdir:"))
						if !filepath.IsAbs(git) {
							git = filepath.Join(dir, git)
						}
					} else {
						git = ""
					}
				}
				if git != "" {
					if data, err := os.ReadFile(filepath.Join(git, "HEAD")); err == nil && strings.HasPrefix(string(data), "ref: refs/heads/") {
						branch = strings.TrimSpace(strings.TrimPrefix(string(data), "ref: refs/heads/"))
					}
				}
			}
		}
		if (foundGit && config.Namespace != "") || filepath.Dir(dir) == dir {
			return branch, config
		}
	}
}

// matchWord receives text folded once by matchTriggers.
func matchWord(word, text string) bool {
	if word == "" {
		return false
	}
	ascii := true
	for _, r := range word {
		if r > 127 {
			ascii = false
			break
		}
	}
	word = strings.ToLower(word)
	if !ascii {
		return strings.Contains(text, word)
	}
	alnum := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' }
	for offset := 0; offset <= len(text)-len(word); {
		i := strings.Index(text[offset:], word)
		if i < 0 {
			return false
		}
		i += offset
		end := i + len(word)
		if (i == 0 || !alnum(text[i-1])) && (end == len(text) || !alnum(text[end])) {
			return true
		}
		offset = i + 1
	}
	return false
}

func matchTriggers(ps []projectEntry, text string) ([]projectEntry, map[string][]string) {
	text = strings.ToLower(text)
	var hits []projectEntry
	words := make(map[string][]string)
	for _, p := range ps {
		if !validNamespace(p.Namespace) {
			continue
		}
		key := projectKey(p)
		for _, word := range p.Triggers {
			if matchWord(word, text) {
				words[key] = append(words[key], word)
			}
		}
		if len(words[key]) > 0 {
			hits = append(hits, p)
		}
	}
	return hits, words
}
