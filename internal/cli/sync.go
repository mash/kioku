package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/internal/wiki"
)

type syncResult struct {
	Namespace string   `json:"namespace"`
	Added     []string `json:"added,omitempty"`
	Updated   []string `json:"updated,omitempty"`
	Deleted   []string `json:"deleted,omitempty"`
}

func (r syncResult) changed() bool {
	return len(r.Added)+len(r.Updated)+len(r.Deleted) > 0
}

// Retry transport failures and temporary server responses, never auth failures.
func (a *App) syncRequest(path string, out any) (int, error) {
	var status int
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		status, err = a.request("GET", path, nil, out)
		if err == nil || status != 0 && status != 429 && status < 500 {
			return status, err
		}
		if attempt < 2 {
			select {
			case <-a.ctx.Done():
				return status, a.ctx.Err()
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
		}
	}
	return status, err
}

// Only the service-owned wiki/maps trees are mirrored. inbox is never touched.
func (a *App) mirrorRoot() (string, error) {
	for _, name := range []string{"wiki", "maps"} {
		if err := ensureDirectory(filepath.Join(a.Dir, name)); err != nil {
			return "", err
		}
	}
	return a.Dir, nil
}

func mirrorPath(root, name string) (string, error) {
	if !wiki.ValidFile(name) || !strings.HasPrefix(name, "wiki/") && !strings.HasPrefix(name, "maps/") {
		return "", errors.New("invalid mirror filename")
	}
	path := filepath.Join(root, filepath.FromSlash(name))
	parent := filepath.Dir(path)
	rel, _ := filepath.Rel(root, parent)
	dir := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		dir = filepath.Join(dir, part)
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("mirror directory is not a real directory")
		}
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return "", errors.New("mirror file is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return path, nil
}

func (a *App) syncWiki() (syncResult, error) {
	result := syncResult{Namespace: a.Config.Namespace}
	endpoint, err := a.projectPath()
	if err != nil {
		return result, err
	}
	if err = a.ensureProject(); err != nil {
		return result, err
	}
	result.Namespace = a.Config.Namespace
	unlock, err := a.localLock()
	if err != nil {
		return result, err
	}
	defer unlock()
	if err := a.saveProjectConfig(); err != nil {
		return result, err
	}
	var remote model.SyncManifest
	if status, e := a.syncRequest(endpoint+"/snapshot?manifest=1", &remote); e != nil {
		if status == 403 || status == 404 {
			return result, errSyncForbidden
		}
		return result, e
	}
	if remote.Files == nil || !safeID(remote.Revision) {
		return result, errors.New("invalid sync manifest")
	}
	var total int64
	for name, f := range remote.Files {
		if !wiki.ValidFile(name) || f.Bytes < 0 || f.Bytes > 64<<20 || len(f.Hash) != 64 {
			return result, errors.New("invalid sync file")
		}
		total += f.Bytes
		if total > 64<<20 {
			return result, errors.New("snapshot exceeds 64 MiB")
		}
	}
	root, err := a.mirrorRoot()
	if err != nil {
		return result, err
	}
	// Enumerate actual files too: local edits and extra generated pages are overwritten/deleted.
	local := map[string]bool{}
	for _, tree := range []string{"wiki", "maps"} {
		err = filepath.WalkDir(filepath.Join(root, tree), func(path string, d os.DirEntry, e error) error {
			if os.IsNotExist(e) {
				return nil
			}
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("symlink in mirror")
			}
			if !d.IsDir() {
				name, _ := filepath.Rel(root, path)
				local[filepath.ToSlash(name)] = true
			}
			return nil
		})
		if err != nil {
			return result, err
		}
	}
	var failures []error
	for _, name := range sortedKeys(local) {
		if _, ok := remote.Files[name]; ok {
			continue
		}
		path, e := mirrorPath(root, name)
		if e == nil {
			e = os.Remove(path)
		}
		if e != nil {
			failures = append(failures, e)
			continue
		}
		result.Deleted = append(result.Deleted, name)
	}
	for _, name := range sortedKeys(remote.Files) {
		path, e := mirrorPath(root, name)
		if e != nil {
			failures = append(failures, e)
			continue
		}
		old, e := os.ReadFile(path)
		if e == nil && auth.Hash(string(old)) == remote.Files[name].Hash {
			continue
		}
		var file model.SnapshotFile
		if _, e = a.syncRequest(endpoint+"/snapshot?file="+url.QueryEscape(name), &file); e != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, e))
			continue
		}
		// A publish may have advanced after the manifest: accept verified current bytes.
		if len(file.Body) > 64<<20 || total-remote.Files[name].Bytes+int64(len(file.Body)) > 64<<20 || auth.Hash(file.Body) != file.Hash {
			failures = append(failures, fmt.Errorf("%s: invalid body hash or size", name))
			continue
		}
		if e = store.AtomicWrite(path, []byte(file.Body), 0400); e != nil {
			failures = append(failures, e)
			continue
		}
		total += int64(len(file.Body)) - remote.Files[name].Bytes
		if local[name] {
			result.Updated = append(result.Updated, name)
		} else {
			result.Added = append(result.Added, name)
		}
	}
	return result, errors.Join(failures...)
}

var errSyncForbidden = errors.New("project access lost")
