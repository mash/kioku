package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mash/kioku/internal/model"
)

// Caller holds the project's gate. Never delete after an ambiguous metadata
// commit: View fails on a poisoned store, leaving the redo journal authoritative.
func (s *Server) discardUpload(ctx context.Context, id string) error {
	var u model.Upload
	if err := s.DB.View(func(st model.State) error { u = st.Uploads[id]; return nil }); err != nil {
		return err
	}
	if u.ID == "" {
		return nil
	}
	var cleanupErr error
	for _, src := range u.Sources {
		for _, key := range []string{src.OriginalKey, src.TextKey} {
			if err := s.Blobs.Delete(ctx, key); err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
	}
	if cleanupErr != nil {
		return fmt.Errorf("upload remains reserved until cleanup succeeds: %w", cleanupErr)
	}
	return s.DB.Update(func(st *model.State) error { delete(st.Uploads, id); return nil })
}
func (s *Server) recoverProjectUploads(ctx context.Context, pid string) error {
	ids := []string{}
	if err := s.DB.View(func(st model.State) error {
		for id, u := range st.Uploads {
			if u.ProjectID == pid {
				ids = append(ids, id)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.discardUpload(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) recoverUploads(ctx context.Context) error {
	ctx, cancelAll := context.WithTimeout(ctx, 2*time.Second)
	defer cancelAll()
	projects := map[string]bool{}
	if err := s.DB.View(func(st model.State) error {
		for _, u := range st.Uploads {
			projects[u.ProjectID] = true
		}
		return nil
	}); err != nil {
		return err
	}
	var result error

	for pid := range projects {
		if ctx.Err() != nil {
			return errors.Join(result, ctx.Err())
		}
		unlock, available := s.tryLockProject(pid)
		if !available {
			continue
		}
		work, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.recoverProjectUploads(work, pid)
		cancel()
		unlock()
		if err != nil {
			result = errors.Join(result, err)
		}
	}

	return result
}
