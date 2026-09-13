package store

import (
	"context"
	"database/sql"
	"fmt"
)

func databaseIsEmpty(ctx context.Context, conn *sql.Conn) (bool, error) {
	const query = `SELECT NOT EXISTS (
		SELECT 1 FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'
	)`
	var empty bool
	if err := conn.QueryRowContext(ctx, query).Scan(&empty); err != nil {
		return false, fmt.Errorf("check database initialization state: %w", err)
	}
	return empty, nil
}

func validateRequiredTables(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT expected.name
		FROM (
			SELECT 'meta' AS name
			UNION ALL SELECT 'submissions'
			UNION ALL SELECT 'idempotency'
			UNION ALL SELECT 'leases'
		) AS expected
		LEFT JOIN sqlite_schema AS actual
		  ON actual.type = 'table' AND actual.name = expected.name
		WHERE actual.name IS NULL
		ORDER BY expected.name`)
	if err != nil {
		return fmt.Errorf("validate state schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("validate state schema: %w", err)
		}
		missing = append(missing, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("validate state schema: %w", err)
	}
	if len(missing) != 0 {
		return fmt.Errorf("%w: required tables are missing: %v", ErrStateUnavailable, missing)
	}
	return nil
}

func validateCurrentSchema(ctx context.Context, conn *sql.Conn) error {
	checks := []string{
		`SELECT key, value FROM meta LIMIT 0`,
		`SELECT submission_id, client_request_id, tool, strategy, descriptor_digest,
			args_hash, args_enc, task_id, status, sequence, events_enc, result_enc,
			error_code, error_message, error_retryable, protocol_version,
			adapter_version, created_at, updated_at, completed_at,
			payload_expires_at, tombstone_expires_at, lease_owner, lease_expires_at
		 FROM submissions LIMIT 0`,
		`SELECT client_request_id, args_hash, submission_id, created_at FROM idempotency LIMIT 0`,
		`SELECT name, owner, expires_at FROM leases LIMIT 0`,
	}
	for _, query := range checks {
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("%w: validate current schema: %v", ErrStateUnavailable, err)
		}
		_ = rows.Close()
	}
	return validatePrimaryKeys(ctx, conn)
}

func validatePrimaryKeys(ctx context.Context, conn *sql.Conn) error {
	expected := []struct {
		table, column string
	}{
		{"meta", "key"},
		{"submissions", "submission_id"},
		{"idempotency", "client_request_id"},
		{"leases", "name"},
	}
	for _, key := range expected {
		rows, err := conn.QueryContext(ctx, "SELECT name, pk FROM pragma_table_info(?) WHERE pk > 0", key.table)
		if err != nil {
			return fmt.Errorf("%w: inspect primary key for %s: %v", ErrStateUnavailable, key.table, err)
		}
		var columns []string
		for rows.Next() {
			var column string
			var position int
			if err := rows.Scan(&column, &position); err != nil {
				_ = rows.Close()
				return fmt.Errorf("%w: inspect primary key for %s: %v", ErrStateUnavailable, key.table, err)
			}
			columns = append(columns, column)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			return fmt.Errorf("%w: inspect primary key for %s: %v", ErrStateUnavailable, key.table, rowsErr)
		}
		if closeErr != nil {
			return fmt.Errorf("%w: close primary key inspection for %s: %v", ErrStateUnavailable, key.table, closeErr)
		}
		if len(columns) != 1 || columns[0] != key.column {
			return fmt.Errorf("%w: table %s must have primary key %s", ErrStateUnavailable, key.table, key.column)
		}
	}
	return nil
}
