package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// schemaVersion is the database schema this build reads and writes.
const schemaVersion = 2

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
	    error_retryable INTEGER NOT NULL DEFAULT 0,
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
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var schema string
	err = conn.QueryRowContext(ctx,
		"SELECT value FROM meta WHERE key = ?", metaSchemaVersion,
	).Scan(&schema)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		empty, checkErr := databaseIsEmpty(ctx, conn)
		if checkErr != nil {
			return checkErr
		}
		if !empty {
			return fmt.Errorf(
				"%w: metadata %q is missing from a nonempty database",
				ErrStateUnavailable,
				metaSchemaVersion,
			)
		}
		if err := s.initializeDatabase(ctx, conn, keys); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("read metadata %q: %w", metaSchemaVersion, err)
	default:
		if err := s.readMeta(ctx, conn, schema); err != nil {
			return err
		}
		if s.schema > schemaVersion {
			return fmt.Errorf("%w: database has %d", ErrUnsupportedSchema, s.schema)
		}
		if err := s.upgradeSchema(ctx, conn); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	committed = true

	if s.schema != schemaVersion {
		return fmt.Errorf("%w: database has %d", ErrUnsupportedSchema, s.schema)
	}
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
	s.cipher, err = newStateCipher(key)
	if err != nil {
		return err
	}
	if s.format < encryptionFormat {
		if err := s.migrateEncryption(ctx); err != nil {
			return err
		}
	}
	return nil
}

func databaseIsEmpty(ctx context.Context, conn *sql.Conn) (bool, error) {
	const query = `
SELECT NOT EXISTS (
    SELECT 1 FROM meta
    UNION ALL SELECT 1 FROM submissions
    UNION ALL SELECT 1 FROM idempotency
    UNION ALL SELECT 1 FROM leases
)`
	var empty bool
	if err := conn.QueryRowContext(ctx, query).Scan(&empty); err != nil {
		return false, fmt.Errorf("check database initialization state: %w", err)
	}
	return empty, nil
}

func (s *Store) upgradeSchema(ctx context.Context, conn *sql.Conn) error {
	if s.schema == 1 {
		if _, err := conn.ExecContext(ctx,
			"ALTER TABLE submissions ADD COLUMN error_retryable INTEGER NOT NULL DEFAULT 0",
		); err != nil {
			return fmt.Errorf("upgrade schema to 2: %w", err)
		}
		if _, err := conn.ExecContext(ctx,
			"UPDATE meta SET value = ? WHERE key = ?", strconv.Itoa(schemaVersion), metaSchemaVersion,
		); err != nil {
			return fmt.Errorf("record schema 2: %w", err)
		}
		s.schema = 2
	}
	return nil
}

// initializeDatabase creates the random key and all metadata while holding the
// database's write lock. Metadata is committed together by migrate; a failed
// key creation leaves no partial database identity. If commit fails after the
// external key write, the unused key is harmless and the next open can retry.
func (s *Store) initializeDatabase(ctx context.Context, conn *sql.Conn, keys KeyProvider) error {
	keyID, key, err := keys.CreateStateKey()
	if err != nil {
		// Fail closed (D14): without the secure backend the initial key cannot
		// be established, so the profile state cannot be secured.
		return fmt.Errorf("%w: create state key: %w", ErrStateUnavailable, err)
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
	s.schema = schemaVersion
	s.format = encryptionFormat
	return nil
}

// readMeta loads the version and key identifier from existing metadata.
func (s *Store) readMeta(ctx context.Context, conn *sql.Conn, schema string) error {
	version, err := parseSchemaVersion(schema)
	if err != nil {
		return err
	}
	s.schema = version

	var keyID string
	if err := conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaStateKeyID).Scan(&keyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("metadata %q is missing", metaStateKeyID)
		}
		return fmt.Errorf("read metadata %q: %w", metaStateKeyID, err)
	}
	s.keyID = keyID

	var format string
	if err := conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaEncryptionFormat).Scan(&format); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("metadata %q is missing", metaEncryptionFormat)
		}
		return fmt.Errorf("read metadata %q: %w", metaEncryptionFormat, err)
	}
	parsedFormat, err := strconv.Atoi(format)
	if err != nil || parsedFormat < 1 || parsedFormat > encryptionFormat {
		return fmt.Errorf("unsupported encryption format %q", format)
	}
	s.format = parsedFormat
	return nil
}

func parseSchemaVersion(value string) (int, error) {
	var version int
	if _, err := fmt.Sscanf(value, "%d", &version); err != nil || version <= 0 {
		return 0, fmt.Errorf("metadata schema version %q is not a positive integer", value)
	}
	return version, nil
}
