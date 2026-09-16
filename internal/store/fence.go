package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// credentialFenceName is the single fenced credential stream per profile
// database: the OAuth refresh credential.
const credentialFenceName = "oauth-refresh"

// ReadCredentialFence returns the generation and the secure-backend label
// of the slot the fence currently points at, or found=false when no
// fenced credential has been committed yet. Writers take generation+1 and
// their own unique slot label; the fence commit makes their slot live.
func (s *Store) ReadCredentialFence(ctx context.Context) (generation int64, slot string, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		"SELECT generation, slot FROM credential_fence WHERE name = ?", credentialFenceName).
		Scan(&generation, &slot)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("%w: read credential fence: %w", ErrStateUnavailable, err)
	}
	return generation, slot, true, nil
}

// CommitCredentialFence atomically points the fence at slot for
// fenceGeneration when, and only when, the fence has not advanced past
// fenceGeneration AND leaseOwner still holds leaseName unexpired in the
// lease ownership epoch leaseGeneration. It is the credential-side
// compare-and-swap: a writer whose fence generation a concurrent commit
// already passed is rejected, and so is a writer that lost the refresh
// lease while its secret-store write was blocked — with a provider that
// permits overlapping rotations, only the live lease owner may advance
// the fence. It returns committed=false for a rejected writer, never an
// error for it.
func (s *Store) CommitCredentialFence(
	ctx context.Context,
	fenceGeneration int64,
	slot, leaseName, leaseOwner string,
	leaseGeneration int64,
) (bool, error) {
	if fenceGeneration <= 0 {
		return false, errors.New("credential fence generation must be positive")
	}
	if slot == "" {
		return false, errors.New("credential fence slot is required")
	}
	if err := validateLease(leaseName, leaseOwner, time.Minute); err != nil {
		return false, err
	}
	nowMs := s.now().UnixMilli()
	res, err := s.exec(ctx, `
		INSERT INTO credential_fence (name, generation, slot)
		SELECT ?, ?, ?
		WHERE EXISTS (
			SELECT 1 FROM leases
			WHERE name = ? AND owner = ? AND generation = ? AND expires_at > ?
		)
		ON CONFLICT(name) DO UPDATE SET
			generation = excluded.generation,
			slot = excluded.slot
		WHERE credential_fence.generation < excluded.generation`,
		credentialFenceName, fenceGeneration, slot,
		leaseName, leaseOwner, leaseGeneration, nowMs)
	if err != nil {
		return false, fmt.Errorf("%w: commit credential fence: %w", ErrStateUnavailable, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%w: commit credential fence: %w", ErrStateUnavailable, err)
	}
	return affected > 0, nil
}

// ClearCredentialFence removes the credential fence row when, and only
// when, leaseOwner still holds leaseName unexpired in the lease ownership
// epoch leaseGeneration. Logout pairs it with deleting the committed slot
// and the legacy labels; binding the clear to the epoch means a logout
// whose lease was lost mid-cleanup can never wipe a newer fence installed
// by the process that took over. It reports cleared=false for a lost
// epoch, never an error for it.
func (s *Store) ClearCredentialFence(
	ctx context.Context,
	leaseName, leaseOwner string,
	leaseGeneration int64,
) (bool, error) {
	if err := validateLease(leaseName, leaseOwner, time.Minute); err != nil {
		return false, err
	}
	res, err := s.exec(ctx, `
		DELETE FROM credential_fence
		WHERE name = ?
		  AND EXISTS (
			  SELECT 1 FROM leases
			  WHERE name = ? AND owner = ? AND generation = ? AND expires_at > ?
		  )`,
		credentialFenceName, leaseName, leaseOwner, leaseGeneration, s.now().UnixMilli())
	if err != nil {
		return false, fmt.Errorf("%w: clear credential fence: %w", ErrStateUnavailable, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%w: clear credential fence: %w", ErrStateUnavailable, err)
	}
	return affected > 0, nil
}

// retiredCredentialSlotName is the credential-slot record name prefix for
// the retirement backlog: slots whose secure-backend deletion failed and
// that a later refresh or logout must retry.
func retiredCredentialSlotName(slot string) string {
	return credentialFenceName + "-retired:" + slot
}

// RecordRetiredCredentialSlot durably records one live credential slot
// whose secure-backend deletion failed, so a later refresh or logout can
// retry the deletion instead of silently stranding a still-valid grant.
func (s *Store) RecordRetiredCredentialSlot(ctx context.Context, slot string) error {
	if slot == "" {
		return errors.New("credential slot is required")
	}
	if _, err := s.exec(ctx, `
		INSERT INTO credential_fence (name, generation, slot) VALUES (?, 0, ?)
		ON CONFLICT(name) DO UPDATE SET slot = excluded.slot`,
		retiredCredentialSlotName(slot), slot); err != nil {
		return fmt.Errorf("%w: record retired credential slot: %w", ErrStateUnavailable, err)
	}
	return nil
}

// RetiredCredentialSlots lists the credential slots recorded for retirement
// retry.
func (s *Store) RetiredCredentialSlots(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT slot FROM credential_fence WHERE name LIKE ?",
		credentialFenceName+"-retired:%")
	if err != nil {
		return nil, fmt.Errorf("%w: list retired credential slots: %w", ErrStateUnavailable, err)
	}
	defer func() { _ = rows.Close() }()
	var slots []string
	for rows.Next() {
		var slot string
		if err := rows.Scan(&slot); err != nil {
			return nil, fmt.Errorf("%w: list retired credential slots: %w", ErrStateUnavailable, err)
		}
		slots = append(slots, slot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: list retired credential slots: %w", ErrStateUnavailable, err)
	}
	return slots, nil
}

// ClearRetiredCredentialSlot removes the retirement record for one slot
// after its deletion succeeded.
func (s *Store) ClearRetiredCredentialSlot(ctx context.Context, slot string) error {
	if slot == "" {
		return errors.New("credential slot is required")
	}
	if _, err := s.exec(ctx,
		"DELETE FROM credential_fence WHERE name = ?", retiredCredentialSlotName(slot)); err != nil {
		return fmt.Errorf("%w: clear retired credential slot: %w", ErrStateUnavailable, err)
	}
	return nil
}
