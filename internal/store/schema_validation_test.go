package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kritama/tama-link/internal/limits"
)

func TestOpenRejectsUnrelatedDatabaseWithoutPollutingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unrelated.db")
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE application_data (value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create unrelated schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close unrelated database: %v", err)
	}

	keys := &testKeys{}
	_, err = Open(context.Background(), path, keys, Config{Limits: limits.Default()})
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("Open unrelated database = %v, want ErrStateUnavailable", err)
	}
	if len(keys.key) != 0 {
		t.Fatal("state key created for unrelated database")
	}

	db, err = sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		t.Fatalf("reopen unrelated database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var tamaTables int
	if err := db.QueryRow(`
		SELECT count(*) FROM sqlite_schema
		WHERE type = 'table' AND name IN ('meta', 'submissions', 'idempotency', 'leases')
	`).Scan(&tamaTables); err != nil {
		t.Fatalf("inspect unrelated schema: %v", err)
	}
	if tamaTables != 0 {
		t.Fatalf("Tama Link tables after rejected open = %d, want 0", tamaTables)
	}
}

func TestOpenRejectsCurrentSchemaWithMissingDurableTable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")
	keys := &testKeys{}
	s, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	if _, err := s.db.Exec("DROP TABLE idempotency"); err != nil {
		t.Fatalf("drop idempotency: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	if _, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()}); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("Open missing durable table = %v, want ErrStateUnavailable", err)
	}
	db, err := sql.Open("sqlite", sqliteURI(path))
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'idempotency'`).Scan(&count); err != nil {
		t.Fatalf("inspect rejected schema: %v", err)
	}
	if count != 0 {
		t.Fatal("Open silently recreated the missing idempotency table")
	}
}

func TestOpenRejectsCurrentSchemaWithoutIdempotencyPrimaryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	keys := &testKeys{}
	s, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	if _, err := s.db.Exec(`
		ALTER TABLE idempotency RENAME TO idempotency_original;
		CREATE TABLE idempotency (
			client_request_id TEXT NOT NULL,
			args_hash TEXT NOT NULL,
			submission_id TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
		DROP TABLE idempotency_original;
	`); err != nil {
		t.Fatalf("remove idempotency primary key: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	if _, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()}); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("Open without idempotency primary key = %v, want ErrStateUnavailable", err)
	}
}

func TestOpenRejectsMalformedSchemaVersions(t *testing.T) {
	for _, version := range []string{"2garbage", "2.0", "2 3", " 2", "2 "} {
		t.Run(version, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			keys := &testKeys{}
			s, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()})
			if err != nil {
				t.Fatalf("create database: %v", err)
			}
			if _, err := s.db.Exec("UPDATE meta SET value = ? WHERE key = ?", version, metaSchemaVersion); err != nil {
				t.Fatalf("corrupt schema version: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("close database: %v", err)
			}

			if _, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()}); err == nil {
				t.Fatalf("Open accepted malformed schema version %q", version)
			}
		})
	}
}

func TestOpenRejectsUnsupportedSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	keys := &testKeys{}
	s, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	// A version newer than the only supported schema fails closed.
	if _, err := s.db.Exec("UPDATE meta SET value = '3' WHERE key = ?", metaSchemaVersion); err != nil {
		t.Fatalf("replace schema version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	if _, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()}); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("Open schema version 3 = %v, want ErrUnsupportedSchema", err)
	}
}

func TestOpenRejectsUnsupportedEncryptionFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	keys := &testKeys{}
	s, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	if _, err := s.db.Exec("UPDATE meta SET value = '2' WHERE key = ?", metaEncryptionFormat); err != nil {
		t.Fatalf("replace encryption format: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	if _, err := Open(context.Background(), path, keys, Config{Limits: limits.Default()}); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("Open encryption format 2 = %v, want ErrStateUnavailable", err)
	}
}
