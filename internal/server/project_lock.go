package server

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mash/kioku/internal/store"
)

func (s *Server) checkDeleteOwner(pid, uid string) error {
	return s.Store.Read(context.Background(), func(q *store.Queries) error { return deleteOwner(q, pid, uid) })
}
func deleteOwner(q *store.Queries, pid, uid string) error {
	role, err := q.ProjectRole(pid, uid)
	if errors.Is(err, sql.ErrNoRows) || err == nil && role != "owner" {
		return fail(404, "project not found")
	}
	if err != nil {
		return err
	}
	general, err := q.IsGeneralProject(pid)
	if err == nil && general {
		return fail(409, "General can only be deleted with your account")
	}
	return err
}
