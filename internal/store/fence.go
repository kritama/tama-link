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

// ClearCredentialFence removes the credential fence row, so the profile
// has no live fenced credential. Logout pairs it with deleting the
// committed slot and the legacy labels: readers then see no credential at
// all instead of following the pointer to a deleted slot and failing with
// a backend error.
func (s *Store) ClearCredentialFence(ctx context.Context) error {
	if _, err := s.exec(ctx, "DELETE FROM credential_fence WHERE name = ?", credentialFenceName); err != nil {
		return fmt.Errorf("%w: clear credential fence: %w", ErrStateUnavailable, err)
	}
	return nil
}
