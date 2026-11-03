package server

import (
	"context"
	"net/http"

	"github.com/mash/kioku/internal/model"
)

// Project work never holds the catalog mutex across IO. A separate,
// cancellation-aware gate serializes publication, uploads and GC for each project.
func (s *Server) lockProject(ctx context.Context, pid string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gate, _ := s.projectLocks.LoadOrStore(pid, make(chan struct{}, 1))
	ch := gate.(chan struct{})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	}
}
func (s *Server) checkOwner(pid, uid string) error {
	return s.DB.View(func(st model.State) error { _, err := owner(st, pid, uid); return err })
}
func (s *Server) checkDeleteOwner(pid, uid string) error {
	return s.DB.View(func(st model.State) error {
		p, ok := st.Projects[pid]
		if !ok || p.Members[uid] != "owner" {
			return fail(http.StatusNotFound, "project not found")
		}
		return nil
	})
}

func (s *Server) tryLockProject(pid string) (func(), bool) {
	gate, _ := s.projectLocks.LoadOrStore(pid, make(chan struct{}, 1))
	ch := gate.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	default:
		return nil, false
	}
}
