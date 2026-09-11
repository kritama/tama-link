package store

import (
	"context"
	"fmt"
	"strconv"
)

type encryptedRow struct {
	id                        string
	arguments, events, result []byte
}

// migrateEncryption upgrades legacy global-AAD blobs while holding a database
// write lock. Each opener rechecks metadata after acquiring the lock, so only
// one process performs the rewrite.
func (s *Store) migrateEncryption(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire encryption migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin encryption migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var format string
	if err := conn.QueryRowContext(ctx,
		"SELECT value FROM meta WHERE key = ?", metaEncryptionFormat,
	).Scan(&format); err != nil {
		return fmt.Errorf("read encryption format: %w", err)
	}
	if format == strconv.Itoa(encryptionFormat) {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("commit encryption migration check: %w", err)
		}
		committed = true
		s.format = encryptionFormat
		return nil
	}
	if format != "1" {
		return fmt.Errorf("unsupported encryption format %q", format)
	}

	rows, err := conn.QueryContext(ctx, "SELECT submission_id, args_enc, events_enc, result_enc FROM submissions")
	if err != nil {
		return fmt.Errorf("read legacy encrypted blobs: %w", err)
	}
	var encrypted []encryptedRow
	for rows.Next() {
		var row encryptedRow
		if err := rows.Scan(&row.id, &row.arguments, &row.events, &row.result); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read legacy encrypted blobs: %w", err)
		}
		encrypted = append(encrypted, row)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy encrypted rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read legacy encrypted blobs: %w", err)
	}
	for _, row := range encrypted {
		arguments, err := s.resealLegacy(row.arguments, row.id, "arguments")
		if err != nil {
			return err
		}
		events, err := s.resealLegacy(row.events, row.id, "events")
		if err != nil {
			return err
		}
		result, err := s.resealLegacy(row.result, row.id, "result")
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `
			UPDATE submissions SET args_enc = ?, events_enc = ?, result_enc = ?
			WHERE submission_id = ?`, arguments, events, result, row.id); err != nil {
			return fmt.Errorf("write upgraded blobs for %s: %w", row.id, err)
		}
	}
	if _, err := conn.ExecContext(ctx,
		"UPDATE meta SET value = ? WHERE key = ?", strconv.Itoa(encryptionFormat), metaEncryptionFormat,
	); err != nil {
		return fmt.Errorf("write encryption format: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit encryption migration: %w", err)
	}
	committed = true
	s.format = encryptionFormat
	return nil
}

func (s *Store) resealLegacy(blob []byte, id, kind string) ([]byte, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	plain, err := s.cipher.openLegacy(blob)
	if err != nil {
		return nil, fmt.Errorf("open legacy %s for %s: %w", kind, id, err)
	}
	sealed, err := s.cipher.seal(plain, id, kind)
	if err != nil {
		return nil, fmt.Errorf("seal upgraded %s for %s: %w", kind, id, err)
	}
	return sealed, nil
}
