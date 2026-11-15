package server

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mash/kioku/internal/store"
)

func (s *Server) lockProject(ctx context.Context, pid string) (func(), error) {
	return s.storage.lock(ctx, pid)
}
func (s *Server) checkOwner(pid, uid string) error {
	return s.Store.Read(context.Background(), func(q *store.Queries) error { _, err := owner(q, pid, uid); return err })
}
func (s *Server) checkDeleteOwner(pid, uid string) error {
	return s.Store.Read(context.Background(), func(q *store.Queries) error {
		role, err := q.ProjectRole(pid, uid)
		if errors.Is(err, sql.ErrNoRows) || err == nil && role != "owner" {
			return fail(404, "project not found")
		}
		return err
	})
}
