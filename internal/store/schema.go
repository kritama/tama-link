package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// schemaVersion is the database schema this build reads and writes.
const schemaVersion = 1

// Metadata keys.
const (
	metaSchemaVersion    = "schema_version"
	metaEncryptionFormat = "encryption_format"
	metaStateKeyID       = "state_key_id"
)

// createSchema creates every table if it does not exist. It is idempotent.
const createSchema = `
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS submissions (
    submission_id TEXT PRIMARY KEY,
    client_request_id TEXT NOT NULL,
    tool TEXT NOT NULL,
    strategy TEXT NOT NULL,
    descriptor_digest TEXT NOT NULL,
    args_hash TEXT NOT NULL,
    args_enc BLOB,
    task_id TEXT,
    status TEXT NOT NULL,
    sequence INTEGER NOT NULL DEFAULT 0,
    events_enc BLOB,
    result_enc BLOB,
    error_code TEXT,
    error_message TEXT,
    protocol_version TEXT NOT NULL DEFAULT '',
    adapter_version TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    completed_at INTEGER,
    payload_expires_at INTEGER,
    tombstone_expires_at INTEGER,
    lease_owner TEXT,
    lease_expires_at INTEGER
);

CREATE TABLE IF NOT EXISTS idempotency (
    client_request_id TEXT PRIMARY KEY,
    args_hash TEXT NOT NULL,
    submission_id TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS leases (
    name TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    expires_at INTEGER NOT NULL
);
`

// migrate creates the schema when absent and establishes the encryption key
// lifecycle: a brand-new database gets a fresh random key; an existing
// database must find its stored key in the credential backend or the open
// fails closed.
func (s *Store) migrate(ctx context.Context, keys KeyProvider) error {
	if _, err := s.db.ExecContext(ctx, createSchema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}

	created, err := s.metaSet(ctx, metaSchemaVersion, fmt.Sprint(schemaVersion))
	if err != nil {
		return err
	}
	if created {
		if _, err := s.metaSet(ctx, metaEncryptionFormat, fmt.Sprint(encryptionFormat)); err != nil {
			return err
		}
		return s.initKey(ctx, keys)
	}

	if err := s.readMeta(ctx); err != nil {
		return err
	}
	if s.schema != schemaVersion {
		return fmt.Errorf("%w: database has %d", ErrUnsupportedSchema, s.schema)
	}

	key, err := keys.GetStateKey(s.keyID)
	if errors.Is(err, ErrKeyMissing) {
		return fmt.Errorf("%w: key %q is missing from the credential backend", ErrStateUnavailable, s.keyID)
	}
	if err != nil {
		return fmt.Errorf("read state key: %w", err)
	}
	s.cipher, err = newStateCipher(key)
	if err != nil {
		return err
	}
	return nil
}

// initKey creates the random key for a brand-new database. If the process
// dies before the metadata commit, the next open simply creates another key;
// the first database never held data, so the orphaned key is harmless.
func (s *Store) initKey(ctx context.Context, keys KeyProvider) error {
	keyID, key, err := keys.CreateStateKey()
	if err != nil {
		return fmt.Errorf("create state key: %w", err)
	}
	if keyID == "" || len(key) != 32 {
		return fmt.Errorf("state key %q must be 32 bytes", keyID)
	}
	if len(keyID) > maxKeyID {
		return fmt.Errorf("state key identifier exceeds %d bytes", maxKeyID)
	}
	c, err := newStateCipher(key)
	if err != nil {
		return err
	}
	s.cipher = c
	if _, err := s.metaSet(ctx, metaStateKeyID, keyID); err != nil {
		return err
	}
	s.keyID = keyID
	return nil
}

// metaSet writes a metadata row unless it already exists. It reports whether
// the row was newly created.
func (s *Store) metaSet(ctx context.Context, key, value string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin metadata write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM meta WHERE key = ?", key).Scan(&exists); err != nil {
		return false, fmt.Errorf("read metadata %q: %w", key, err)
	}
	if exists > 0 {
		_ = tx.Rollback()
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES (?, ?)", key, value); err != nil {
		return false, fmt.Errorf("write metadata %q: %w", key, err)
	}
	return true, tx.Commit()
}

// readMeta loads the version and key identifier from existing metadata.
func (s *Store) readMeta(ctx context.Context) error {
	var schema string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaSchemaVersion).Scan(&schema); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("metadata %q is missing", metaSchemaVersion)
		}
		return fmt.Errorf("read metadata %q: %w", metaSchemaVersion, err)
	}
	version, err := parseSchemaVersion(schema)
	if err != nil {
		return err
	}
	s.schema = version

	var keyID string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaStateKeyID).Scan(&keyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("metadata %q is missing", metaStateKeyID)
		}
		return fmt.Errorf("read metadata %q: %w", metaStateKeyID, err)
	}
	s.keyID = keyID
	return nil
}

func parseSchemaVersion(value string) (int, error) {
	var version int
	if _, err := fmt.Sscanf(value, "%d", &version); err != nil || version <= 0 {
		return 0, fmt.Errorf("metadata schema version %q is not a positive integer", value)
	}
	return version, nil
}
