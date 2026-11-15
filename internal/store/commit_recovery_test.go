package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
)

func TestCommitRecoverySurvivesRequestCancellation(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(strconv.FormatBool(committed), func(t *testing.T) {
			d := newCatalog(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			d.commit = func(tx *sql.Tx) error {
				var err error
				if committed {
					err = tx.Commit()
				} else {
					err = tx.Rollback()
				}
				if err != nil {
					t.Fatal(err)
				}
				cancel()
				return errors.New("response lost")
			}
			err := d.Write(ctx, func(q *Queries) error { return q.PutUser(UserRow{ID: "target", Email: "one"}) })
			var unknown *UnknownCommit
			if !errors.As(err, &unknown) {
				t.Fatal(err)
			}
			inspected := false
			if err := d.ResolveCommit(ctx, func(q *Queries) error {
				inspected = true
				_, err := q.User("target")
				if committed {
					return err
				}
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("unexpected commit: %v", err)
				}
				return nil
			}); err != nil || !inspected {
				t.Fatalf("caller cancellation prevented commit recovery: inspected=%v err=%v", inspected, err)
			}
		})
	}
}
