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

// CredentialFenceCommit describes one fence advancement: the new slot, the
// previous live slot to enqueue for retirement retry, and the refresh lease
// epoch the writer must still own.
type CredentialFenceCommit struct {
	// FenceGeneration is the generation the writer commits: it must be
	// greater than the current fence generation.
	FenceGeneration int64
	// Slot is the new live credential slot.
	Slot string
	// PreviousSlot is the live credential slot this commit replaces. It is
	// atomically enqueued for retirement retry with the fence advance, so
	// a later deletion failure can never strand the old grant.
	PreviousSlot string
	// LeaseName, LeaseOwner, and LeaseGeneration identify the lease epoch
	// the writer must still own, unexpired.
	LeaseName       string
	LeaseOwner      string
	LeaseGeneration int64
}

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

// CommitCredentialFence atomically advances the fence to commit.Slot for
// commit.FenceGeneration and enqueues commit.PreviousSlot for retirement
// retry, when, and only when, the fence has not advanced past
// commit.FenceGeneration AND commit.LeaseOwner still holds
// commit.LeaseName unexpired in the lease ownership epoch
// commit.LeaseGeneration. It is the credential-side compare-and-swap: a
// writer whose fence generation a concurrent commit already passed is
// rejected, and so is a writer that lost the refresh lease while its
// secret-store write was blocked — with a provider that permits
// overlapping rotations, only the live lease owner may advance the fence.
// The enqueue is one transaction with the advance, so a committed fence
// always carries a durable retirement record for the slot it replaced and
// a failed deletion can never strand the old grant. It returns
// committed=false for a rejected writer, never an error for it.
func (s *Store) CommitCredentialFence(ctx context.Context, commit CredentialFenceCommit) (bool, error) {
	if commit.FenceGeneration <= 0 {
		return false, errors.New("credential fence generation must be positive")
	}
	if commit.Slot == "" {
		return false, errors.New("credential fence slot is required")
	}
	if err := validateLease(commit.LeaseName, commit.LeaseOwner, time.Minute); err != nil {
		return false, err
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: commit credential fence: %w", ErrStateUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Verify the lease epoch before touching the fence: the check must
	// gate the plain INSERT path too, because a fence row that logout
	// cleared or a first-ever authorization leaves the upsert without an
	// existing row, and a stale writer whose SetSecret blocked past its
	// lease would otherwise reinstall credentials behind the logout.
	var held int
	err = tx.QueryRowContext(ctx,
		"SELECT 1 FROM leases WHERE name = ? AND owner = ? AND generation = ? AND expires_at > ?",
		commit.LeaseName, commit.LeaseOwner, commit.LeaseGeneration, s.now().UnixMilli()).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: commit credential fence: %w", ErrStateUnavailable, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO credential_fence (name, generation, slot) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			generation = excluded.generation,
			slot = excluded.slot
		WHERE credential_fence.generation < excluded.generation`,
		credentialFenceName, commit.FenceGeneration, commit.Slot)
	if err != nil {
		return false, fmt.Errorf("%w: commit credential fence: %w", ErrStateUnavailable, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%w: commit credential fence: %w", ErrStateUnavailable, err)
	}
	if affected == 0 {
		return false, nil
	}
	if commit.PreviousSlot != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO credential_fence (name, generation, slot) VALUES (?, 0, ?)
			ON CONFLICT(name) DO UPDATE SET slot = excluded.slot`,
			retiredCredentialSlotName(commit.PreviousSlot), commit.PreviousSlot); err != nil {
			return false, fmt.Errorf("%w: record retired credential slot: %w", ErrStateUnavailable, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%w: commit credential fence: %w", ErrStateUnavailable, err)
	}
	return true, nil
}

// ClearCredentialFence removes the credential fence row when, and only
// when, leaseOwner still holds leaseName unexpired in the lease ownership
// epoch leaseGeneration, and when retiredSlot is non-empty, enqueues it
// for retirement retry in the same transaction, so a pointer that is
// cleared always leaves a durable reference to the slot it pointed at:
// a crash or a later deletion failure can never strand the slot. Logout
// passes an empty slot (it deletes the committed slot while the fence
// still references it); the invalid-grant cleanup passes the fenced slot.
// Binding the clear to the epoch means a cleanup whose lease was lost
// mid-flight can never wipe a newer fence installed by the process that
// took over. An absent fence row is successfully cleared — a repeated
// logout or a legacy-only profile has nothing to clear — so only a lost
// epoch reports cleared=false.
func (s *Store) ClearCredentialFence(
	ctx context.Context,
	leaseName, leaseOwner string,
	leaseGeneration int64,
	retiredSlot string,
) (bool, error) {
	if err := validateLease(leaseName, leaseOwner, time.Minute); err != nil {
		return false, err
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: clear credential fence: %w", ErrStateUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	var held int
	err = tx.QueryRowContext(ctx,
		"SELECT 1 FROM leases WHERE name = ? AND owner = ? AND generation = ? AND expires_at > ?",
		leaseName, leaseOwner, leaseGeneration, s.now().UnixMilli()).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: clear credential fence: %w", ErrStateUnavailable, err)
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM credential_fence WHERE name = ?", credentialFenceName); err != nil {
		return false, fmt.Errorf("%w: clear credential fence: %w", ErrStateUnavailable, err)
	}
	if retiredSlot != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO credential_fence (name, generation, slot) VALUES (?, 0, ?)
			ON CONFLICT(name) DO UPDATE SET slot = excluded.slot`,
			retiredCredentialSlotName(retiredSlot), retiredSlot); err != nil {
			return false, fmt.Errorf("%w: record retired credential slot: %w", ErrStateUnavailable, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%w: clear credential fence: %w", ErrStateUnavailable, err)
	}
	return true, nil
}

// retiredCredentialSlotName is the credential-slot record name prefix for
// the retirement backlog: slots whose secure-backend deletion failed and
// that a later refresh or logout must retry.
func retiredCredentialSlotName(slot string) string {
	return credentialFenceName + "-retired:" + slot
}

// invalidatedCredentialName is the durable invalidation marker: it records
// that the profile's legacy refresh credential holds a grant the
// authorization server rejected, so the legacy label must not count as
// live even when its deletion fails.
const invalidatedCredentialName = credentialFenceName + "-invalidated"

// MarkRefreshCredentialInvalidated durably records that the profile's
// legacy refresh credential is a known-invalid grant. The invalid-grant
// cleanup marks before it deletes the legacy label, so a failed deletion
// cannot bring the rejected grant back to life through the fence-less
// legacy fallback; the marker is cleared once the deletion succeeds or a
// new credential commits.
func (s *Store) MarkRefreshCredentialInvalidated(ctx context.Context) error {
	if _, err := s.exec(ctx, `
		INSERT INTO credential_fence (name, generation, slot) VALUES (?, 0, '')
		ON CONFLICT(name) DO UPDATE SET slot = ''`,
		invalidatedCredentialName); err != nil {
		return fmt.Errorf("%w: mark refresh credential invalidated: %w", ErrStateUnavailable, err)
	}
	return nil
}

// ClearRefreshCredentialInvalidation removes the invalidation marker after
// the legacy deletion succeeds or a new credential commits.
func (s *Store) ClearRefreshCredentialInvalidation(ctx context.Context) error {
	if _, err := s.exec(ctx,
		"DELETE FROM credential_fence WHERE name = ?", invalidatedCredentialName); err != nil {
		return fmt.Errorf("%w: clear refresh credential invalidation: %w", ErrStateUnavailable, err)
	}
	return nil
}

// RefreshCredentialInvalidated reports whether the invalidation marker is
// present, meaning the legacy label holds a known-invalid grant that must
// not be treated as a live credential.
func (s *Store) RefreshCredentialInvalidated(ctx context.Context) (bool, error) {
	var held int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM credential_fence WHERE name = ?", invalidatedCredentialName).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: read refresh credential invalidation: %w", ErrStateUnavailable, err)
	}
	return true, nil
}

// RecordRetiredCredentialSlot durably records one live credential slot
// whose secure-backend deletion failed, so a later refresh or logout can
// retry the deletion instead of silently stranding the blob in the
// backend. The fence-advance enqueue path uses the atomic commit; this
// covers the cleanup paths that delete without a commit.
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
