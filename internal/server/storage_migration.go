package server

import (
	"context"
	"fmt"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/wiki"
)

// Old installations stored immutable build directories. Migrate the selected
// wiki once into the current tree; retain no page-history records afterwards.
func (s *Server) migrateWikiStorage() error {
	type candidate struct {
		project  model.Project
		selected model.Revision
		count    int
	}
	var candidates []candidate
	if err := s.DB.View(func(st model.State) error {
		counts := map[string]int{}
		for _, r := range st.Revisions {
			counts[r.ProjectID]++
		}
		for _, p := range st.Projects {
			if p.Deleting {
				continue
			}
			selected := st.Revisions[p.Revision]
			needs := counts[p.ID] > 1
			for name, key := range selected.Files {
				if key != blob.Key("projects/"+p.ID+"/"+name) {
					needs = true
				}
			}
			if needs {
				candidates = append(candidates, candidate{p, selected, counts[p.ID]})
			}
		}
		return nil
	}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, item := range candidates {
		p, selected := item.project, item.selected
		next := selected
		next.Files = map[string]blob.Key{}
		next.FileBytes = map[string]int64{}
		next.FileHashes = map[string]string{}
		next.StoredBytes = 0
		for name, key := range selected.Files {
			if !wiki.ValidFile(name) || name == "wiki/inbox.md" {
				return fmt.Errorf("invalid legacy wiki filename %q", name)
			}
			body, err := s.Blobs.Get(ctx, key)
			if err != nil {
				return err
			}
			current := blob.Key("projects/" + p.ID + "/" + name)
			if err = s.Blobs.Put(ctx, current, body); err != nil {
				return err
			}
			next.Files[name] = current
			next.FileBytes[name] = int64(len(body))
			next.FileHashes[name] = auth.Hash(string(body))
			next.StoredBytes += int64(len(body))
		}
		if err := s.DB.Update(func(st *model.State) error {
			for id, r := range st.Revisions {
				if r.ProjectID == p.ID {
					delete(st.Revisions, id)
				}
			}
			if next.ID != "" {
				st.Revisions[next.ID] = next
			}
			return nil
		}); err != nil {
			return err
		}
		// The service is not listening yet; normal GC can remove old build objects.
		keys, err := s.Blobs.List(ctx, "projects/"+p.ID+"/revisions/")
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err = s.Blobs.Delete(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}
