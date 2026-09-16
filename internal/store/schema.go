package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// schemaVersion is the database schema this build reads and writes. Version
// 2 adds the input_responses table for request-correlated input replay;
// version 3 adds the lease generation counter used to gate credential
// writes behind one lease holder.
const schemaVersion = 3

// Metadata keys.
const (
	metaSchemaVersion    = "schema_version"
	metaEncryptionFormat = "encryption_format"
	metaStateKeyID       = "state_key_id"
)

// createSchema creates every table for a brand-new empty database. Existing
// databases are validated without repairing missing objects.
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
    error_retryable INTEGER NOT NULL DEFAULT 0,
    protocol_version TEXT NOT NULL DEFAULT '',
    adapter_version TEXT NOT NULL DEFAULT '',
    response_bytes INTEGER NOT NULL DEFAULT 16777216,
    result_bytes INTEGER NOT NULL DEFAULT 8388608,
    event_bytes INTEGER NOT NULL DEFAULT 16384,
    max_events INTEGER NOT NULL DEFAULT 128,
    events_bytes INTEGER NOT NULL DEFAULT 1048576,
    payload_retention_ms INTEGER NOT NULL DEFAULT 604800000,
    tombstone_retention_ms INTEGER NOT NULL DEFAULT 2592000000,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    completed_at INTEGER,
    payload_expires_at INTEGER,
    tombstone_expires_at INTEGER,
    lease_owner TEXT,
    lease_expires_at INTEGER
);

CREATE TABLE IF NOT EXISTS input_responses (
    submission_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    response_enc BLOB NOT NULL,
    answered_at INTEGER NOT NULL,
    PRIMARY KEY (submission_id, request_id)
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
    expires_at INTEGER NOT NULL,
    generation INTEGER NOT NULL DEFAULT 1
);
`

// initializeOrValidate creates the initial schema when absent and establishes
// the encryption key lifecycle. An existing database must match the only
// supported initial schema and format and find its stored key in the credential
// backend or the open fails closed.
func (s *Store) initializeOrValidate(ctx context.Context, keys KeyProvider) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire state connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin state initialization: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	empty, err := databaseIsEmpty(ctx, conn)
	if err != nil {
		return err
	}
	if empty {
		if _, err := conn.ExecContext(ctx, createSchema); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
		if err := s.initializeDatabase(ctx, conn, keys); err != nil {
			return err
		}
	} else {
		if err := validateRequiredTables(ctx, conn); err != nil {
			return err
		}
		var schema string
		if err := conn.QueryRowContext(ctx,
			"SELECT value FROM meta WHERE key = ?", metaSchemaVersion,
		).Scan(&schema); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: metadata %q is missing", ErrStateUnavailable, metaSchemaVersion)
			}
			return fmt.Errorf("%w: read metadata %q: %w", ErrStateUnavailable, metaSchemaVersion, err)
		}
		if err := s.validateMeta(ctx, conn, schema); err != nil {
			return err
		}
		if err := validateCurrentSchema(ctx, conn); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit state initialization: %w", err)
	}
	committed = true

	key, found, err := keys.GetStateKey(s.keyID)
	if err != nil {
		// Fail closed (D14): a missing key or an unavailable backend both make
		// the encrypted state unreadable. Never generate a replacement key or
		// fall back to plaintext.
		return fmt.Errorf("%w: key %q: %w", ErrStateUnavailable, s.keyID, err)
	}
	if !found {
		return fmt.Errorf("%w: key %q is missing from the credential backend", ErrStateUnavailable, s.keyID)
	}
	if len(key) != stateKeySize {
		return fmt.Errorf("%w: key %q must be %d bytes, got %d", ErrStateUnavailable, s.keyID, stateKeySize, len(key))
	}
	s.cipher, err = newStateCipher(key)
	if err != nil {
		return err
	}
	return nil
}

// initializeDatabase creates the random key and all metadata while holding the
// database's write lock. Metadata is committed together by
// initializeOrValidate; a failed key creation leaves no partial database
// identity. If commit fails after the external key write, the unused key is
// harmless and the next open can retry.
func (s *Store) initializeDatabase(ctx context.Context, conn *sql.Conn, keys KeyProvider) error {
	keyID, key, err := keys.CreateStateKey()
	if err != nil {
		// Fail closed (D14): without the secure backend the initial key cannot
		// be established, so the profile state cannot be secured.
		return fmt.Errorf("%w: create state key: %w", ErrStateUnavailable, err)
	}
	if keyID == "" || len(key) != stateKeySize {
		return fmt.Errorf("%w: state key %q must be %d bytes", ErrStateUnavailable, keyID, stateKeySize)
	}
	if len(keyID) > maxKeyID {
		return fmt.Errorf("%w: state key identifier exceeds %d bytes", ErrStateUnavailable, maxKeyID)
	}
	c, err := newStateCipher(key)
	if err != nil {
		return err
	}
	s.cipher = c
	metadata := []struct{ key, value string }{
		{metaSchemaVersion, strconv.Itoa(schemaVersion)},
		{metaEncryptionFormat, strconv.Itoa(encryptionFormat)},
		{metaStateKeyID, keyID},
	}
	for _, item := range metadata {
		if _, err := conn.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES (?, ?)", item.key, item.value); err != nil {
			return fmt.Errorf("write metadata %q: %w", item.key, err)
		}
	}
	s.keyID = keyID
	return nil
}

// validateMeta validates the persisted versions and loads the key identifier.
func (s *Store) validateMeta(ctx context.Context, conn *sql.Conn, schema string) error {
	version, err := parseSchemaVersion(schema)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStateUnavailable, err)
	}
	if version != schemaVersion {
		return fmt.Errorf("%w: database has %d", ErrUnsupportedSchema, version)
	}

	var keyID string
	if err := conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaStateKeyID).Scan(&keyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: metadata %q is missing", ErrStateUnavailable, metaStateKeyID)
		}
		return fmt.Errorf("%w: read metadata %q: %w", ErrStateUnavailable, metaStateKeyID, err)
	}
	if keyID == "" || len(keyID) > maxKeyID {
		return fmt.Errorf("%w: metadata %q is invalid", ErrStateUnavailable, metaStateKeyID)
	}
	s.keyID = keyID

	var format string
	if err := conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaEncryptionFormat).Scan(&format); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: metadata %q is missing", ErrStateUnavailable, metaEncryptionFormat)
		}
		return fmt.Errorf("%w: read metadata %q: %w", ErrStateUnavailable, metaEncryptionFormat, err)
	}
	parsedFormat, err := strconv.Atoi(format)
	if err != nil || parsedFormat != encryptionFormat {
		return fmt.Errorf("%w: unsupported encryption format %q", ErrStateUnavailable, format)
	}
	return nil
}

func parseSchemaVersion(value string) (int, error) {
	version, err := strconv.Atoi(value)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("metadata schema version %q is not a positive integer", value)
	}
	return version, nil
}
