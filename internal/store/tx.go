package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
)

// writeTx is one IMMEDIATE write transaction on a dedicated pooled
// connection.
//
// The pinned SQLite driver honors busy timeouts when acquiring the write
// lock at BEGIN time, but not when a write statement inside a deferred
// transaction upgrades to the writer mid-transaction: that path fails
// immediately with SQLITE_BUSY even though another process would release
// the lock well before the busy timeout. Every multi-statement write
// transaction must therefore begin IMMEDIATE so cross-process contention
// waits instead of failing. The reads that precede the first write share
// the held write lock and always see committed data.
type writeTx struct {
	conn *sql.Conn
	done bool
}

// beginWriteTx starts one IMMEDIATE write transaction.
func (s *Store) beginWriteTx(ctx context.Context) (*writeTx, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, wrapBusy(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, wrapBusy(err)
	}
	return &writeTx{conn: conn}, nil
}

func (w *writeTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res, err := w.conn.ExecContext(ctx, query, args...)
	return res, wrapBusy(err)
}

// exec runs one autocommit write statement, tagging any write-lock
// timeout for callers that can defer the work.
func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res, err := s.db.ExecContext(ctx, query, args...)
	return res, wrapBusy(err)
}

// wrapBusy tags a write-lock timeout so callers can distinguish a
// transiently busy store from a real state failure.
func wrapBusy(err error) error {
	if err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") {
		return err
	}
	return fmt.Errorf("%w: %v", ErrBusy, err)
}

func (w *writeTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return w.conn.QueryContext(ctx, query, args...)
}

func (w *writeTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return w.conn.QueryRowContext(ctx, query, args...)
}

// Commit finishes the transaction and returns the connection to the pool.
// The pinned SQLite driver executes COMMIT as raw SQL and never reaches the
// driver.Tx path that rolls back automatically after a failed COMMIT, so a
// failed commit rolls back explicitly; if that rollback also fails, the
// connection is marked bad so database/sql discards it instead of returning
// an active transaction to the pool.
func (w *writeTx) Commit() error {
	_, err := w.conn.ExecContext(context.Background(), "COMMIT")
	if err == nil {
		w.done = true
		return w.conn.Close()
	}
	if rbErr := w.Rollback(); rbErr != nil {
		// The connection still owns an active transaction the rollback
		// could not clear: mark it bad before releasing it.
		_ = w.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	w.done = true
	return err
}

// Rollback undoes the transaction and returns the connection to the pool.
// A rollback after commit is a no-op, matching sql.Tx semantics at the
// deferred call sites. A failed ROLLBACK marks the connection bad so
// database/sql discards it instead of returning an active transaction to
// the pool.
func (w *writeTx) Rollback() error {
	if w.done {
		return nil
	}
	_, err := w.conn.ExecContext(context.Background(), "ROLLBACK")
	if err != nil {
		_ = w.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	w.done = true
	_ = w.conn.Close()
	return err
}
