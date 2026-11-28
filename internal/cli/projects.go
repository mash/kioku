package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mash/kioku/internal/atomicfile"
	"github.com/mash/kioku/internal/model"
)

type projectEntry struct {
	General     bool     `json:"general,omitempty"`
	Triggers    []string `json:"triggers,omitempty"`
	Server      string   `json:"server"`
	Namespace   string   `json:"namespace"`
	Path        string   `json:"path"`
	Description string   `json:"description,omitempty"`
}

type projectsFile struct {
	Projects []projectEntry `json:"projects"`
}

type invalidProjectsError struct{ error }

func readProjects(path string) (*projectsFile, error) {
	b, err := readRegular(path)
	if os.IsNotExist(err) {
		return &projectsFile{Projects: []projectEntry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseProjects(path, b)
}

func parseProjects(path string, b []byte) (*projectsFile, error) {
	p := &projectsFile{Projects: []projectEntry{}}
	if b == nil {
		return p, nil
	}
	if err := json.Unmarshal(b, p); err != nil {
		return nil, &invalidProjectsError{fmt.Errorf("%s: %w", path, err)}
	}
	for _, entry := range p.Projects {
		if !filepath.IsAbs(entry.Path) {
			return nil, &invalidProjectsError{fmt.Errorf("%s: project path must be absolute", path)}
		}
	}
	return p, nil
}

func (p *projectsFile) find(server, namespace string) int {
	for i, entry := range p.Projects {
		if entry.Server == server && entry.Namespace == namespace {
			return i
		}
	}
	return -1
}

func (a *App) projectsPath() string {
	return filepath.Join(filepath.Dir(a.AuthDir), "projects.json")
}

// Resolve the normalized origin without a lock; localLock always comes first.
func (a *App) resolveProjectDir(origin, namespace string) (string, error) {
	path := a.projectsPath()
	projects, err := readProjects(path)
	if err != nil {
		return "", err
	}
	var dir string
	if i := projects.find(origin, namespace); i >= 0 {
		dir = projects.Projects[i].Path
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "share", "kioku", "projects", filepath.FromSlash(namespace))
	}
	if err := ensureDirectory(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Call after localLock. Lock the directory so atomicfile.Write cannot replace the
// locked inode, and no separate lock file is needed.
func updateProjects(path string, change func(*projectsFile) bool) error {
	return updateLockedFile(path, func(data []byte) ([]byte, bool, error) {
		p, err := parseProjects(path, data)
		if err != nil {
			return nil, false, err
		}
		if !change(p) {
			return nil, false, nil
		}
		b, err := json.MarshalIndent(p, "", "  ")
		return append(b, '\n'), true, err
	})
}

// updateLockedFile locks the directory across reading and atomic replacement.
func updateLockedFile(path string, change func([]byte) ([]byte, bool, error)) error {
	if err := ensureDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := openLockedFile(filepath.Dir(path), os.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := readRegular(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	next, write, err := change(data)
	if err != nil || !write {
		return err
	}
	return atomicfile.Write(path, next, 0600)
}

func (a *App) addProjectEntry(server, namespace, dir string, description *string) error {
	path := a.projectsPath()
	return updateProjects(path, func(p *projectsFile) bool {
		i := p.find(server, namespace)
		added := i < 0
		if added {
			p.Projects = append(p.Projects, projectEntry{Server: server, Namespace: namespace, Path: dir})
			i = len(p.Projects) - 1
		} else if existing := p.Projects[i].Path; existing != dir && a.Err != nil {
			fmt.Fprintf(a.Err, "warning: %s already lists %s at %s; keeping that path\n", path, namespace, existing)
		}
		if description != nil && p.Projects[i].Description != *description {
			p.Projects[i].Description = *description
			return true
		}
		return added
	})
}

// Only update bound mirrors. Listing does not create or bind local directories.
func (a *App) updateGeneralFlags(list []model.ProjectEntry) error {
	general := make(map[string]bool)
	for _, project := range list {
		if project.General {
			general[project.Namespace] = true
		}
	}
	return updateProjects(a.projectsPath(), func(p *projectsFile) bool {
		changed := false
		for i := range p.Projects {
			entry := &p.Projects[i]
			if entry.Server == a.Config.Server && entry.General != general[entry.Namespace] {
				entry.General = general[entry.Namespace]
				changed = true
			}
		}
		return changed
	})
}

func (a *App) deleteProjectEntry() error {
	return updateProjects(a.projectsPath(), func(p *projectsFile) bool {
		kept := p.Projects[:0]
		for _, entry := range p.Projects {
			if entry.Server != a.Config.Server || entry.Namespace != a.Config.Namespace {
				kept = append(kept, entry)
			}
		}
		changed := len(kept) != len(p.Projects)
		p.Projects = kept
		return changed
	})
}

// Invalid input requires repair; a write failure must not turn a successful
// binding, sync or deletion into a failed operation.
func (a *App) projectEntryResult(err error, recovery string) error {
	var invalid *invalidProjectsError
	if errors.As(err, &invalid) {
		return err
	}
	if err != nil && a.Err != nil {
		fmt.Fprintf(a.Err, "warning: could not update projects.json: %v; %s\n", err, recovery)
	}
	return nil
}

// foldProjects preserves file order and the first path for each projectKey.
func foldProjects(ps []projectEntry) []projectEntry {
	descriptions := make(map[string]string)
	for _, p := range ps {
		if descriptions[p.Namespace] == "" {
			descriptions[p.Namespace] = p.Description
		}
	}
	var result []projectEntry
	indexes := make(map[string]int)
	for _, p := range ps {
		triggers := p.Triggers
		key := projectKey(p)
		i, exists := indexes[key]
		if !exists {
			i = len(result)
			indexes[key] = i
			p.Description = descriptions[p.Namespace]
			p.Triggers = nil
			result = append(result, p)
		}
		words, err := normalizeTriggers(append(append([]string{}, result[i].Triggers...), triggers...))
		if err == nil {
			result[i].Triggers = words
		}
	}
	return result
}
