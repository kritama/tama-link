package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// generation when, and only when, the fence has not advanced past
// generation. It is the credential-side compare-and-swap: a writer whose
// generation a concurrent commit already passed is rejected, so a stale
// writer can never make its value live. It returns committed=false for a
// stale writer, never an error for it.
func (s *Store) CommitCredentialFence(ctx context.Context, generation int64, slot string) (bool, error) {
	if generation <= 0 {
		return false, errors.New("credential fence generation must be positive")
	}
	if slot == "" {
		return false, errors.New("credential fence slot is required")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO credential_fence (name, generation, slot) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			generation = excluded.generation,
			slot = excluded.slot
		WHERE credential_fence.generation < excluded.generation`,
		credentialFenceName, generation, slot)
	if err != nil {
		return false, fmt.Errorf("%w: commit credential fence: %w", ErrStateUnavailable, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%w: commit credential fence: %w", ErrStateUnavailable, err)
	}
	return affected > 0, nil
}
