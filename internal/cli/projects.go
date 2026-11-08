package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mash/kioku/internal/store"
)

type projectEntry struct {
	Server      string `json:"server"`
	Namespace   string `json:"namespace"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type projectsFile struct {
	Projects []projectEntry `json:"projects"`
}

type invalidProjectsError struct{ error }

func readProjects(path string) (*projectsFile, error) {
	p := &projectsFile{Projects: []projectEntry{}}
	b, err := readRegular(path)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return nil, err
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

// Call after localLock. Lock the directory so AtomicWrite cannot replace the
// locked inode, and no separate lock file is needed.
func updateProjects(path string, change func(*projectsFile) bool) error {
	if err := ensureDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := openLockedFile(filepath.Dir(path), os.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	p, err := readProjects(path)
	if err != nil {
		return err
	}
	if !change(p) {
		return nil
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return store.AtomicWrite(path, append(b, '\n'), 0600)
}

func (a *App) addProjectEntry(description *string) error {
	path := a.projectsPath()
	return updateProjects(path, func(p *projectsFile) bool {
		i := p.find(a.Config.Server, a.Config.Namespace)
		added := i < 0
		if added {
			p.Projects = append(p.Projects, projectEntry{Server: a.Config.Server, Namespace: a.Config.Namespace, Path: a.Dir})
			i = len(p.Projects) - 1
		} else if existing := p.Projects[i].Path; existing != a.Dir && a.Err != nil {
			fmt.Fprintf(a.Err, "warning: %s already lists %s at %s; keeping that path\n", path, a.Config.Namespace, existing)
		}
		if description != nil && p.Projects[i].Description != *description {
			p.Projects[i].Description = *description
			return true
		}
		return added
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
