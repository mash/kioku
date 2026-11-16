package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
)

// Queries is bound to exactly one SQL transaction and never holds a State map.
type Queries struct {
	tx  *sql.Tx
	ctx context.Context
}

func (d *Database) Read(ctx context.Context, fn func(*Queries) error) error {
	return stopOnBrokenCatalog(transact(ctx, d.reader, fn))
}
func (d *Database) Write(ctx context.Context, fn func(*Queries) error) error {
	return stopOnBrokenCatalog(transact(ctx, d.writer, fn))
}

// exitProcess is replaced in tests.
var exitProcess = os.Exit

// stopOnBrokenCatalog stops the process on SQLITE_IOERR, SQLITE_CORRUPT and
// SQLITE_NOTADB. The supervisor restarts kiokud, and startup runs quick_check.
// Other errors, such as busy or constraint failures, roll back the transaction.
func stopOnBrokenCatalog(err error) error {
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		// SQLite extended codes retain the primary result in their low byte.
		switch coded.Code() & 255 {
		case 10, 11, 26:
			slog.Error("SQLite catalog failed; stopping", "error", err)
			exitProcess(1)
		}
	}
	return err
}
func transact(ctx context.Context, db *sql.DB, fn func(*Queries) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(&Queries{tx: tx, ctx: ctx}); err != nil {
		return err
	}
	return tx.Commit()
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
