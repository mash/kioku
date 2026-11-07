package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/wiki"
)

type publishInput struct {
	Job      model.Job
	Project  model.Project
	Files    map[string]string
	Validate func(model.State) error
	Finish   func(*model.State, time.Time) error
}

// publishWiki runs with the caller holding the project's generation lock.
func (s *Server) publishWiki(ctx context.Context, in publishInput) (string, error) {
	job, project, files := in.Job, in.Project, in.Files
	validate := in.Validate
	if validate == nil {
		validate = func(model.State) error { return nil }
	}
	var published model.Revision
	err := s.DB.View(func(st model.State) error { published = st.Revisions[project.Revision]; return nil })
	if err != nil {
		return "", err
	}
	rev := model.Revision{ID: auth.Token()[:20], ProjectID: job.ProjectID, Created: time.Now().UTC(), Files: map[string]blob.Key{}, FileBytes: map[string]int64{}, FileHashes: map[string]string{}}
	for _, body := range files {
		rev.StoredBytes += int64(len(body))
	}
	// This token is for synchronization only. It does not create a build directory
	// or retain old pages. Mark incomplete before the first mutable object write.
	err = s.DB.Update(func(st *model.State) error {
		if err := validate(*st); err != nil {
			return err
		}
		p, err := member(*st, job.ProjectID, job.UserID)
		if err != nil {
			return err
		}
		p.WikiIncomplete = true
		st.Projects[p.ID] = p
		return nil
	})
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if !wiki.ValidFile(name) {
			return "", fmt.Errorf("invalid generated filename")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		publishUnlock, err := s.lockProject(ctx, job.ProjectID)
		if err != nil {
			return "", err
		}
		err = func() error {
			defer publishUnlock()
			if err := s.DB.View(validate); err != nil {
				return err
			}
			key := blob.Key("projects/" + job.ProjectID + "/" + name)
			// An unchanged file needs no upload. Current metadata is updated per file,
			// so partial progress remains readable and accountable after a failure.
			changed := true
			if oldKey := published.Files[name]; oldKey != "" {
				if published.FileHashes[name] == auth.Hash(body) && oldKey == key {
					changed = false
				}
				if published.FileHashes[name] == "" {
					if b, getErr := s.Blobs.Get(ctx, oldKey); getErr == nil && string(b) == body && oldKey == key {
						changed = false
					}
				}
			}
			if changed {
				if err := s.Blobs.Put(ctx, key, []byte(body)); err != nil {
					return err
				}
			}
			rev.Files[name] = key
			rev.FileBytes[name] = int64(len(body))
			rev.FileHashes[name] = auth.Hash(body)
			return s.DB.Update(func(st *model.State) error {
				if err := validate(*st); err != nil {
					return err
				}
				p, err := member(*st, job.ProjectID, job.UserID)
				if err != nil {
					return err
				}
				current := st.Revisions[p.Revision]
				if current.Files == nil {
					current = model.Revision{ID: rev.ID, ProjectID: p.ID, Files: map[string]blob.Key{}}
				}
				current.Files[name] = key
				if current.FileBytes == nil {
					current.FileBytes = map[string]int64{}
				}
				if current.FileHashes == nil {
					current.FileHashes = map[string]string{}
				}
				current.FileBytes[name] = int64(len(body))
				current.FileHashes[name] = auth.Hash(body)
				current.StoredBytes = 0
				for _, size := range current.FileBytes {
					current.StoredBytes += size
				}
				current.ID = rev.ID
				if changed {
					p.WikiUpdatedAt = time.Now().UTC()
				}
				for id, r := range st.Revisions {
					if r.ProjectID == p.ID {
						delete(st.Revisions, id)
					}
				}
				st.Revisions[rev.ID] = current
				p.Revision = rev.ID
				st.Projects[p.ID] = p
				return nil
			})
		}()
		if err != nil {
			return "", err
		}
	}
	publishUnlock, err := s.lockProject(ctx, job.ProjectID)
	if err != nil {
		return "", err
	}
	defer publishUnlock()
	if err = s.DB.View(validate); err != nil {
		return "", err
	}
	// Delete obsolete pages after writing replacements.
	keys, err := s.Blobs.List(ctx, "projects/"+job.ProjectID+"/")
	if err != nil {
		return "", err
	}
	for _, key := range keys {
		name := strings.TrimPrefix(string(key), "projects/"+job.ProjectID+"/")
		if (strings.HasPrefix(name, "wiki/") || strings.HasPrefix(name, "maps/")) && files[name] == "" {
			if _, ok := files[name]; !ok {
				if err = s.Blobs.Delete(ctx, key); err != nil {
					return "", err
				}
			}
		}
	}
	err = s.DB.Update(func(st *model.State) error {
		if err := validate(*st); err != nil {
			return err
		}
		p, err := member(*st, job.ProjectID, job.UserID)
		if err != nil {
			return err
		}
		for id, r := range st.Revisions {
			if r.ProjectID == p.ID {
				delete(st.Revisions, id)
			}
		}
		st.Revisions[rev.ID] = rev
		p.Revision = rev.ID
		p.WikiUpdatedAt = time.Now().UTC()
		p.WikiIncomplete = false
		st.Projects[p.ID] = p
		if in.Finish != nil {
			if err := in.Finish(st, time.Now().UTC()); err != nil {
				return err
			}
		}
		j := st.Jobs[job.ID]
		j.Status = "completed"
		j.Revision = rev.ID
		st.Jobs[job.ID] = j
		return nil
	})
	return rev.ID, err
}
