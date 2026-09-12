package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

type schemaMigration func(context.Context, *sql.Conn) error

// upgradeSchema applies every intermediate migration in order. The caller
// owns the write transaction, so each schema change and its version marker
// commit atomically.
func (s *Store) upgradeSchema(ctx context.Context, conn *sql.Conn) error {
	for s.schema < schemaVersion {
		migration, ok := schemaMigrationFrom(s.schema)
		if !ok {
			return fmt.Errorf("%w: no migration from database schema %d", ErrUnsupportedSchema, s.schema)
		}
		from := s.schema
		if err := migration(ctx, conn); err != nil {
			return fmt.Errorf("upgrade schema %d to %d: %w", from, from+1, err)
		}
		s.schema++
		if _, err := conn.ExecContext(ctx,
			"UPDATE meta SET value = ? WHERE key = ?", strconv.Itoa(s.schema), metaSchemaVersion,
		); err != nil {
			return fmt.Errorf("record schema %d: %w", s.schema, err)
		}
	}
	return nil
}

func schemaMigrationFrom(version int) (schemaMigration, bool) {
	switch version {
	case 1:
		return migrateSchema1To2, true
	default:
		return nil, false
	}
}

func migrateSchema1To2(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx,
		"ALTER TABLE submissions ADD COLUMN error_retryable INTEGER NOT NULL DEFAULT 0",
	)
	return err
}
