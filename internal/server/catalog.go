package server

import (
	"context"
	"errors"

	"github.com/mash/kioku/internal/store"
)

// catalog exposes transaction boundaries without exposing connections to callers.
type catalog interface {
	Read(context.Context, func(*store.Queries) error) error
	Write(context.Context, func(*store.Queries) error) error
	Close() error
}

// writeConsuming commits fn's writes even when fn rejects the request with an
// apiError, so a single-use code stays consumed after a wrong attempt. Other
// errors roll back the transaction.
func writeConsuming(ctx context.Context, db catalog, fn func(*store.Queries) error) error {
	var rejected error
	if err := db.Write(ctx, func(q *store.Queries) error {
		err := fn(q)
		var api apiError
		if errors.As(err, &api) {
			rejected = err
			return nil
		}
		return err
	}); err != nil {
		return err
	}
	return rejected
}
