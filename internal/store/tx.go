package store

import (
	"context"
	"database/sql"
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
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &writeTx{conn: conn}, nil
}

func (w *writeTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return w.conn.ExecContext(ctx, query, args...)
}

func (w *writeTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return w.conn.QueryContext(ctx, query, args...)
}

func (w *writeTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return w.conn.QueryRowContext(ctx, query, args...)
}

// Commit finishes the transaction and returns the connection to the pool.
func (w *writeTx) Commit() error {
	w.done = true
	_, err := w.conn.ExecContext(context.Background(), "COMMIT")
	_ = w.conn.Close()
	return err
}

// Rollback undoes the transaction and returns the connection to the pool.
// A rollback after commit is a no-op, matching sql.Tx semantics at the
// deferred call sites.
func (w *writeTx) Rollback() error {
	if w.done {
		return nil
	}
	w.done = true
	_, err := w.conn.ExecContext(context.Background(), "ROLLBACK")
	_ = w.conn.Close()
	return err
}
