// Package store is Tama Link's durable local state: an encrypted SQLite
// database per profile holding submissions, the idempotency index, and
// cross-process leases. SQLite is authoritative for the client-facing
// submission lifecycle; the database file is a regular file created 0600 and
// is never followed through symlinks.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	// Registers the pure-Go SQLite driver, which keeps CGO_ENABLED=0 builds
	// working.
	_ "modernc.org/sqlite"

	"github.com/kritama/tama-link/internal/limits"
)

// Errors returned by store operations.
var (
	// ErrNotFound reports that a submission does not exist in this profile.
	ErrNotFound = errors.New("submission not found")
	// ErrStateUnavailable reports that the profile state cannot be secured or
	// read: its key is missing from the credential backend, the backend is
	// unavailable, or the initial key could not be created. The store fails
	// closed and never falls back to plaintext or a replacement key (D14).
	ErrStateUnavailable = errors.New("state unavailable")
	// ErrIdempotencyConflict reports that client_request_id was reused with
	// different canonical input.
	ErrIdempotencyConflict = errors.New("idempotency conflict")
	// ErrConcurrentUpdate reports that another process changed a submission
	// after it was read and the caller must reload before deciding what to do.
	ErrConcurrentUpdate = errors.New("concurrent submission update")
	// ErrLeaseNotOwned reports that a lease-guarded write no longer belongs to
	// the caller or its lease has expired.
	ErrLeaseNotOwned = errors.New("submission lease not owned")
	// ErrResultTooLarge reports that a terminal result exceeds the profile
	// result bound. The result is never truncated or stored.
	ErrResultTooLarge = errors.New("result too large")
	// ErrUnsupportedSchema reports a database schema this build cannot read.
	ErrUnsupportedSchema = errors.New("unsupported schema version")
)

// Config controls one store open.
type Config struct {
	// Limits are the profile's validated effective bounds.
	Limits limits.Limits
	// Now supplies the clock. The zero value uses time.Now.
	Now func() time.Time
}

// Store is one open profile state database. A Store is safe for concurrent
// use by multiple goroutines and may be opened by multiple processes.
type Store struct {
	db     *sql.DB
	cipher stateCipher
	limits limits.Limits
	schema int
	keyID  string
	format int
	now    func() time.Time
	closed bool
}

// Open opens (creating when absent) the profile state database at path.
// The key provider supplies the state encryption key.
func Open(ctx context.Context, path string, keys KeyProvider, cfg Config) (*Store, error) {
	if err := ensureStateFile(path); err != nil {
		return nil, err
	}
	if err := cfg.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("store limits: %w", err)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	sqlDB, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open state database %s: %w", path, err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("open state database %s: %w", path, err)
	}

	s := &Store{db: sqlDB, limits: cfg.Limits, now: now}
	if err := s.migrate(ctx, keys); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// ensureStateFile enforces the secure-file rules before opening: the path
// must not be a directory or a special file, an existing database must be a
// regular file (never a symlink) restricted to 0600, and a missing file is
// created 0600.
func ensureStateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read state database %s: %w", path, err)
		}
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
		if createErr != nil {
			return fmt.Errorf("create state database %s: %w", path, createErr)
		}
		_ = file.Close()
		return nil
	}
	if info.IsDir() {
		return fmt.Errorf("state database %s is a directory", path)
	}
	if info.Mode()&os.ModeType != 0 {
		return fmt.Errorf("state database %s is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("restrict state database %s: %w", path, err)
		}
	}
	return nil
}

// dsn configures busy handling, WAL journaling, and durable-but-fast
// synchronous writes for the pure-Go driver.
func dsn(path string) string {
	return "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
}
