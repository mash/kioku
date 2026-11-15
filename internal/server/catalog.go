package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/mash/kioku/internal/store"
)

// catalog exposes transaction boundaries without exposing connections to callers.
type catalog interface {
	Read(context.Context, func(*store.Queries) error) error
	Write(context.Context, func(*store.Queries) error) error
	ResolveCommit(context.Context, func(*store.Queries) error) error
	Close() error
}

// resolveExchange never regenerates or returns a secret after an ambiguous commit.
func (s *Server) resolveExchange(ctx context.Context, err error, status int) error {
	var fatal *store.FatalDatabaseError
	if errors.As(err, &fatal) {
		s.reportFatal(err)
		return err
	}
	var unknown *store.UnknownCommit
	if !errors.As(err, &unknown) {
		return err
	}
	if e := s.Store.ResolveCommit(ctx, func(q *store.Queries) error { return q.Healthy() }); e != nil {
		s.reportFatal(e)
		return e
	}
	return fail(status, "exchange outcome unknown; sign in again")
}
func (s *Server) reportFatal(err error) {
	var fatal *store.FatalDatabaseError
	if errors.As(err, &fatal) {
		s.fatalOnce.Do(func() { s.fatalError.Store(fatal); close(s.fatal) })
	}
}
func (s *Server) databaseError(ctx context.Context, err error) error {
	var fatal *store.FatalDatabaseError
	if errors.As(err, &fatal) {
		s.reportFatal(err)
		return err
	}
	var unknown *store.UnknownCommit
	if errors.As(err, &unknown) {
		if e := s.Store.ResolveCommit(ctx, func(q *store.Queries) error { return q.Healthy() }); e != nil {
			err = e
		} else {
			return fail(503, "storage commit outcome unknown; retry the request")
		}
	}
	s.reportFatal(err)
	return err
}
func (s *Server) requestError(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, s.databaseError(r.Context(), err))
}
