package store

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func TestPendingAgentCleanupRemainsUntilEligibleForHistory(t *testing.T) {
	d := newCatalog(t)
	catalogWrite(t, d, func(q *Queries) error {
		j := testJob("pending", "p", "completed", 0)
		j.Data.Run = &RunData{}
		j.Phase = pointer("finalizing")
		j.CleanupComplete = pointer(false)
		if e := q.PutJob(j); e != nil {
			return e
		}
		for i := range 257 {
			if e := q.PutJob(testJob(fmt.Sprintf("recent-%03d", i), "p", "completed", 1)); e != nil {
				return e
			}
		}
		return q.CompactHistory("p", nil)
	})
	catalogWrite(t, d, func(q *Queries) error {
		j, e := q.Job("pending")
		if e != nil {
			return e
		}
		j.CleanupComplete = pointer(true)
		if e = q.PutJob(j); e != nil {
			return e
		}
		return q.CompactHistory("p", nil)
	})
	if e := d.Read(t.Context(), func(q *Queries) error {
		_, e := q.Job("pending")
		if !errors.Is(e, sql.ErrNoRows) {
			t.Fatal("eligible old job retained", e)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
