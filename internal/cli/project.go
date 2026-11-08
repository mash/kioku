package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mash/kioku/internal/model"
)

func (a *App) projectPath() (string, error) {
	if !validNamespace(a.Config.Namespace) {
		return "", errors.New("no project configured; run kioku -n organization/project pull")
	}
	return "/api/projects/" + a.Config.Namespace, nil
}

func safeID(s string) bool {
	if len(s) < 1 || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func (a *App) selectProject(p model.Project) error {
	a.Config.Namespace = p.Namespace
	if err := a.saveProjectConfig(); err != nil {
		return fmt.Errorf("remote operation succeeded for %s, but saving .kioku/config failed: %w; retry kioku --server %s -n %s pull after fixing the local path", p.Namespace, err, a.Config.Server, p.Namespace)
	}
	if err := a.ensureProject(); err != nil {
		return fmt.Errorf("remote operation succeeded for %s and configuration was saved, but local setup failed: %w; fix the local path and run kioku pull", p.Namespace, err)
	}
	fmt.Fprintf(a.Out, "%s\n%s\n", p.Namespace, a.localDir())
	return nil
}

func (a *App) localDir() string    { return a.Dir }
func (a *App) metadataDir() string { return filepath.Join(a.Dir, ".kioku") }

func (a *App) ensureProject() error {
	if _, err := a.projectPath(); err != nil {
		return err
	}
	if !validNamespace(a.Config.Namespace) {
		return errors.New("no project configured; run kioku -n organization/project pull")
	}
	for _, dir := range []string{a.metadataDir(), filepath.Join(a.Dir, "inbox"), filepath.Join(a.Dir, "wiki"), filepath.Join(a.Dir, "maps")} {
		if err := ensureDirectory(dir); err != nil {
			return err
		}
	}
	return nil
}

func ensureDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("project directory must be a real directory: %s", dir)
	}
	return nil
}

func validNamespace(s string) bool {
	parts := strings.Split(s, "/")
	return len(parts) == 2 && safeSlug(parts[0]) && safeSlug(parts[1])
}

// Preflight local paths before create/join can mutate cloud membership. Actual
// config persistence remains atomic after the server returns its namespace.
func (a *App) preflightProjectStorage() error {
	if err := a.checkUnboundMirror(); err != nil {
		return err
	}
	for _, path := range []string{a.metadataDir(), filepath.Join(a.Dir, "wiki"), filepath.Join(a.Dir, "maps"), filepath.Join(a.Dir, "inbox")} {
		if err := ensureDirectory(path); err != nil {
			return err
		}
	}
	probe, err := os.CreateTemp(a.metadataDir(), ".config-probe-")
	if err != nil {
		return err
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Remove(name)
}

// Call under localLock before binding or mutating remote membership. A saved
// config declares wiki/maps to be mirrors; without one, existing content is owned
// by the user and must not be adopted by a failed or retried command.
func (a *App) checkUnboundMirror() error {
	current, err := a.readProjectConfig()
	if err != nil {
		return err
	}
	if current.Server != "" {
		return nil
	}
	for _, name := range []string{"wiki", "maps"} {
		path := filepath.Join(a.Dir, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("mirror directory is not a real directory: %s", path)
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("refusing to bind non-empty wiki/maps without .kioku/config")
		}
	}
	return nil
}
