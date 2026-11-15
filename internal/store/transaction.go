package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Queries is bound to exactly one SQL transaction and never holds a State map.
type Queries struct {
	tx  *sql.Tx
	ctx context.Context
}

// CommittedRejection is for the three specified consume/replay-then-reject
// cases. Ordinary errors always roll back, including SQL failures.
type CommittedRejection struct{ Reason error }

func (e *CommittedRejection) Error() string { return e.Reason.Error() }
func (e *CommittedRejection) Unwrap() error { return e.Reason }
func RejectAfterCommit(err error) error     { return &CommittedRejection{Reason: err} }

type UnknownCommit struct{ Cause error }

func (e *UnknownCommit) Error() string { return "SQLite commit outcome unknown" }
func (e *UnknownCommit) Unwrap() error { return e.Cause }

func (d *Database) Read(ctx context.Context, fn func(*Queries) error) error {
	return classifyDatabaseError(transact(ctx, d.reader, false, nil, fn))
}
func (d *Database) Write(ctx context.Context, fn func(*Queries) error) error {
	return classifyDatabaseError(transact(ctx, d.writer, true, d.commit, fn))
}

func classifyDatabaseError(err error) error {
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		// SQLite extended codes retain the primary result in their low byte.
		switch coded.Code() & 255 {
		case 11, 26: // SQLITE_CORRUPT, SQLITE_NOTADB
			return &FatalDatabaseError{Cause: err}
		}
	}
	return err
}
func transact(ctx context.Context, db *sql.DB, write bool, commit func(*sql.Tx) error, fn func(*Queries) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = fn(&Queries{tx: tx, ctx: ctx})
	var declined *CommittedRejection
	if err != nil && (!write || !errors.As(err, &declined)) {
		return err
	}
	if commit == nil {
		commit = (*sql.Tx).Commit
	}
	if commitErr := commit(tx); commitErr != nil {
		if write {
			return &UnknownCommit{Cause: commitErr}
		}
		return commitErr
	}
	return err
}
func (q *Queries) exec(query string, args ...any) (sql.Result, error) {
	return q.tx.ExecContext(q.ctx, query, args...)
}
func (q *Queries) row(query string, args ...any) *sql.Row {
	return q.tx.QueryRowContext(q.ctx, query, args...)
}
func (q *Queries) rows(query string, args ...any) (*sql.Rows, error) {
	return q.tx.QueryContext(q.ctx, query, args...)
}
func marshalData(v any) (string, error)     { b, err := json.Marshal(v); return string(b), err }
func unmarshalData(raw string, v any) error { return json.Unmarshal([]byte(raw), v) }

// ResolveCommit opens one fresh connection and reads the affected rows. It does
// not retry the mutation or regenerate a secret response.
func (d *Database) ResolveCommit(ctx context.Context, inspect func(*Queries) error) error {
	// The caller may disconnect after COMMIT. Recovery must still distinguish
	// a healthy catalog from a failed connection, without repeating the write.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	db, err := catalogConnection(d.path, false, true)
	if err != nil {
		return &FatalDatabaseError{Cause: err}
	}
	defer db.Close()
	if err = transact(ctx, db, false, nil, inspect); err != nil {
		return &FatalDatabaseError{Cause: err}
	}
	return nil
}

type FatalDatabaseError struct{ Cause error }

func (e *FatalDatabaseError) Error() string { return "SQLite connection recovery failed" }
func (e *FatalDatabaseError) Unwrap() error { return e.Cause }
