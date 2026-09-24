package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/kritama/tama-link/internal/contract"
)

// maxCapabilitySnapshot bounds the non-secret capability snapshot stored
// beside a task. A larger document is a protocol failure, not a truncated
// audit record.
const maxCapabilitySnapshot = 64 * 1024

// TaskRecord is the non-secret observation persisted for one owner-bound
// task, plus the encrypted outstanding input-request map. TTL and poll
// interval are the validated millisecond integers, not time.Duration values.
type TaskRecord struct {
	TTLMs          int64
	PollIntervalMs int64
	Capabilities   string
	UpdatedAt      string
	// InputRequests replaces the outstanding map. Nil clears it.
	InputRequests []byte
}

// SaveTaskRecordLeased persists the latest task observation while owner
// holds the live submission lease. A terminal row is left unchanged.
func (s *Store) SaveTaskRecordLeased(
	ctx context.Context,
	id, leaseName, owner string,
	record TaskRecord,
) error {
	if id == "" || leaseName == "" || owner == "" {
		return errors.New("submission id, lease name, and owner are required")
	}
	if len(record.Capabilities) > maxCapabilitySnapshot {
		return fmt.Errorf("task capability snapshot exceeds %d bytes", maxCapabilitySnapshot)
	}
	if record.TTLMs < 0 || record.PollIntervalMs < 0 {
		return errors.New("task ttl and poll interval must be non-negative")
	}
	var sealed []byte
	if len(record.InputRequests) > 0 {
		var err error
		sealed, err = s.cipher.seal(record.InputRequests, id, "input-requests")
		if err != nil {
			return fmt.Errorf("seal input requests: %w", err)
		}
	}

	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return fmt.Errorf("begin task observation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.requireLiveLease(ctx, tx, leaseIdentity{name: leaseName, owner: owner}); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE submissions
		SET task_ttl_ms = ?, task_poll_interval_ms = ?, task_capabilities = ?,
		    task_updated_at = ?, input_requests_enc = ?, updated_at = ?
		WHERE submission_id = ? AND status NOT IN (?, ?, ?, ?, ?)`,
		record.TTLMs, record.PollIntervalMs,
		record.Capabilities, record.UpdatedAt, sealed, s.now().UnixMilli(),
		id,
		string(contract.StatusCompleted), string(contract.StatusFailed),
		string(contract.StatusCancelled), string(contract.StatusExpired),
		string(contract.StatusOutcomeUnknown),
	)
	if err != nil {
		return fmt.Errorf("save task observation: %w", err)
	}
	if err := requireUpdated(res); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task observation: %w", err)
	}
	return nil
}
