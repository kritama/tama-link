package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

type encryptedRow struct {
	rowID                     int64
	id                        string
	arguments, events, result []byte
}

// encryptionMigrationBatchSize bounds the non-payload bookkeeping retained
// while a legacy database is rewritten. Payloads are fetched and resealed one
// row at a time so retained terminal results cannot multiply this bound.
const encryptionMigrationBatchSize = 16

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

	if err := s.resealLegacyRows(ctx, conn); err != nil {
		return err
	}
	updated, err := conn.ExecContext(ctx,
		"UPDATE meta SET value = ? WHERE key = ?", strconv.Itoa(encryptionFormat), metaEncryptionFormat,
	)
	if err != nil {
		return fmt.Errorf("write encryption format: %w", err)
	}
	if err := requireUpdated(updated); err != nil {
		return fmt.Errorf("write encryption format: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit encryption migration: %w", err)
	}
	committed = true
	s.format = encryptionFormat
	return nil
}

func (s *Store) resealLegacyRows(ctx context.Context, conn *sql.Conn) error {
	lastRowID := int64(-1 << 63)
	for {
		rows, err := conn.QueryContext(ctx, `
			SELECT rowid, submission_id
			FROM submissions
			WHERE rowid > ?
			ORDER BY rowid
			LIMIT ?`, lastRowID, encryptionMigrationBatchSize)
		if err != nil {
			return fmt.Errorf("read legacy encrypted row identifiers: %w", err)
		}
		batch := make([]encryptedRow, 0, encryptionMigrationBatchSize)
		for rows.Next() {
			var row encryptedRow
			if err := rows.Scan(&row.rowID, &row.id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read legacy encrypted row identifier: %w", err)
			}
			batch = append(batch, row)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			return fmt.Errorf("read legacy encrypted row identifiers: %w", rowsErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close legacy encrypted row identifiers: %w", closeErr)
		}
		if len(batch) == 0 {
			return nil
		}
		for _, row := range batch {
			if err := conn.QueryRowContext(ctx, `
				SELECT args_enc, events_enc, result_enc
				FROM submissions WHERE rowid = ?`, row.rowID,
			).Scan(&row.arguments, &row.events, &row.result); err != nil {
				return fmt.Errorf("read legacy encrypted blobs for %s: %w", row.id, err)
			}
			if err := s.resealLegacyRow(ctx, conn, row); err != nil {
				return err
			}
			lastRowID = row.rowID
		}
	}
}

func (s *Store) resealLegacyRow(ctx context.Context, conn *sql.Conn, row encryptedRow) error {
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
	updated, err := conn.ExecContext(ctx, `
		UPDATE submissions SET args_enc = ?, events_enc = ?, result_enc = ?
		WHERE rowid = ?`, arguments, events, result, row.rowID)
	if err != nil {
		return fmt.Errorf("write upgraded blobs for %s: %w", row.id, err)
	}
	if err := requireUpdated(updated); err != nil {
		return fmt.Errorf("write upgraded blobs for %s: %w", row.id, err)
	}
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
