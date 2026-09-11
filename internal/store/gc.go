package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// GCSummary reports one retention sweep.
type GCSummary struct {
	// Expired is the number of submissions whose terminal payload fell
	// out of retention and became payload-free tombstones.
	Expired int
	// Deleted is the number of tombstones whose retention lapsed.
	Deleted int
}

// GC applies the retention policy: terminal submissions past payload
// retention become payload-free expired tombstones, and tombstones past
// tombstone retention are deleted with their idempotency entries. Non-terminal
// submissions are never touched.
func (s *Store) GC(ctx context.Context) (GCSummary, error) {
	nowMs := s.now().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GCSummary{}, fmt.Errorf("begin gc: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var summary GCSummary
	if summary, err = s.expirePayloads(ctx, tx, nowMs); err != nil {
		return GCSummary{}, err
	}
	if summary.Deleted, err = s.deleteLapsedTombstones(ctx, tx, nowMs); err != nil {
		return GCSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return GCSummary{}, fmt.Errorf("commit gc: %w", err)
	}
	return summary, nil
}

// expirePayloads converts terminal submissions past payload retention into
// payload-free expired tombstones.
func (s *Store) expirePayloads(ctx context.Context, tx *sql.Tx, nowMs int64) (GCSummary, error) {
	query := `
		UPDATE submissions
		SET status = ?, events_enc = NULL, result_enc = NULL, updated_at = ?
		WHERE completed_at IS NOT NULL
		  AND payload_expires_at IS NOT NULL
		  AND payload_expires_at <= ?
		  AND status != ?`
	res, err := tx.ExecContext(ctx, query,
		"expired", nowMs, nowMs, "expired")
	if err != nil {
		return GCSummary{}, fmt.Errorf("expire payloads: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return GCSummary{}, fmt.Errorf("expire payloads: %w", err)
	}
	return GCSummary{Expired: int(affected)}, nil
}

// deleteLapsedTombstones removes submissions past tombstone retention and
// their idempotency entries.
func (s *Store) deleteLapsedTombstones(ctx context.Context, tx *sql.Tx, nowMs int64) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT submission_id, client_request_id
		FROM submissions
		WHERE tombstone_expires_at IS NOT NULL AND tombstone_expires_at <= ?`, nowMs)
	if err != nil {
		return 0, fmt.Errorf("select lapsed tombstones: %w", err)
	}
	defer func() { _ = rows.Close() }()

	deleted := 0
	for rows.Next() {
		var id, clientRequestID string
		if err := rows.Scan(&id, &clientRequestID); err != nil {
			return 0, fmt.Errorf("select lapsed tombstones: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM submissions WHERE submission_id = ?", id); err != nil {
			return 0, fmt.Errorf("delete submission %s: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM idempotency WHERE client_request_id = ?", clientRequestID,
		); err != nil {
			return 0, fmt.Errorf("delete idempotency entry %s: %w", clientRequestID, err)
		}
		deleted++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("select lapsed tombstones: %w", err)
	}
	return deleted, nil
}

// CheckIntegrity runs SQLite's integrity check and reports its result.
func (s *Store) CheckIntegrity(ctx context.Context) (string, error) {
	var result string
	err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("integrity check returned no rows")
		}
		return "", fmt.Errorf("integrity check: %w", err)
	}
	if !strings.EqualFold(result, "ok") {
		return result, fmt.Errorf("integrity check failed: %s", result)
	}
	return result, nil
}
