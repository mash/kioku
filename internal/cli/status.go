package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/model"
)

func (a *App) status() error {
	if err := a.ensureProject(); err != nil {
		return err
	}
	r, err := a.readReceipts()
	if err != nil {
		return err
	}
	changed := []string{}
	err = filepath.WalkDir(filepath.Join(a.localDir(), "inbox"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if auth.Hash(string(b)) != r.Hashes[abs] {
			rel, _ := filepath.Rel(a.localDir(), p)
			changed = append(changed, filepath.Join(a.localDir(), rel))
		}
		return nil
	})
	if err != nil {
		return err
	}
	var local manifest
	if b, err := os.ReadFile(filepath.Join(a.metadataDir(), "manifest.json")); err == nil {
		if err = json.Unmarshal(b, &local); err != nil {
			return err
		}
	}
	result := map[string]any{"namespace": a.Config.Namespace, "server": a.Config.Server, "local_revision": local.Revision, "local_sync_incomplete": local.Incomplete, "pending_files": changed}
	var projects []model.ProjectEntry
	if _, err = a.request("GET", "/api/projects", nil, &projects); err != nil {
		return err
	}
	found := false
	for _, p := range projects {
		if p.ID == a.Config.Project {
			result["inbox"] = p.Inbox
			result["inbox_count"] = p.InboxCount
			result["remote_revision"] = p.Revision
			result["wiki_updated_at"] = p.WikiUpdatedAt
			result["wiki_incomplete"] = p.WikiIncomplete
			result["next_generate_at"] = p.NextGenerateAt
			found = true
		}
	}
	if !found {
		return errors.New("project no longer accessible")
	}
	return a.print(result)
}
