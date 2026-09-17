package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ClaimLease atomically acquires the named lease for owner. A lease is
// acquired when it does not exist, is expired, or is already held by owner.
// It reports whether owner holds the lease after the call. A claim by a
// different owner advances the lease generation; a same-owner re-claim does
// not, so the generation identifies one ownership epoch.
func (s *Store) ClaimLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	if err := validateLease(name, owner, ttl); err != nil {
		return false, err
	}
	now := s.now()
	nowMs := now.UnixMilli()
	expiresAt := leaseDeadlineMilliseconds(now, ttl)
	res, err := s.exec(ctx, `
		INSERT INTO leases (name, owner, expires_at, generation) VALUES (?, ?, ?, 1)
		ON CONFLICT(name) DO UPDATE SET
			owner = excluded.owner,
			expires_at = excluded.expires_at,
			generation = CASE WHEN leases.owner = excluded.owner
				THEN leases.generation
				ELSE leases.generation + 1 END
		WHERE leases.expires_at <= ? OR leases.owner = excluded.owner`, name, owner, expiresAt, nowMs)
	if err != nil {
		return false, fmt.Errorf("claim lease %q: %w", name, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim lease %q: %w", name, err)
	}
	return affected == 1 && s.now().UnixMilli() < expiresAt, nil
}

// RenewLease extends the named lease. It reports whether owner holds the
// lease after the call.
func (s *Store) RenewLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	if err := validateLease(name, owner, ttl); err != nil {
		return false, err
	}
	now := s.now()
	nowMs := now.UnixMilli()
	expiresAt := leaseDeadlineMilliseconds(now, ttl)
	res, err := s.exec(ctx,
		"UPDATE leases SET expires_at = ? WHERE name = ? AND owner = ? AND expires_at > ?",
		expiresAt, name, owner, nowMs)
	if err != nil {
		return false, fmt.Errorf("renew lease %q: %w", name, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("renew lease %q: %w", name, err)
	}
	return affected == 1 && s.now().UnixMilli() < expiresAt, nil
}

// LeaseGeneration returns the current generation of the named lease when
// owner holds it and it has not expired, or ok=false.
func (s *Store) LeaseGeneration(ctx context.Context, name, owner string) (int64, bool, error) {
	if err := validateLease(name, owner, time.Minute); err != nil {
		return 0, false, err
	}
	var generation int64
	err := s.db.QueryRowContext(ctx,
		"SELECT generation FROM leases WHERE name = ? AND owner = ? AND expires_at > ?",
		name, owner, s.now().UnixMilli()).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read lease generation %q: %w", name, err)
	}
	return generation, true, nil
}

// CommitLease atomically verifies that owner still holds the named lease in
// the same ownership epoch. It performs no state change; callers use it as
// a generation gate before an out-of-store write, so a lease lost to a
// foreign claim fails the gate instead of letting a stale writer commit.
func (s *Store) CommitLease(ctx context.Context, name, owner string, generation int64) (bool, error) {
	if err := validateLease(name, owner, time.Minute); err != nil {
		return false, err
	}
	res, err := s.exec(ctx,
		"UPDATE leases SET expires_at = expires_at WHERE name = ? AND owner = ? AND generation = ? AND expires_at > ?",
		name, owner, generation, s.now().UnixMilli())
	if err != nil {
		return false, fmt.Errorf("commit lease %q: %w", name, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected for lease %q: %w", name, err)
	}
	return affected == 1, nil
}

// ReleaseLease removes the named lease when owner holds it.
func (s *Store) ReleaseLease(ctx context.Context, name, owner string) error {
	res, err := s.exec(ctx, "DELETE FROM leases WHERE name = ? AND owner = ?", name, owner)
	if err != nil {
		return fmt.Errorf("release lease %q: %w", name, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("release lease %q: %w", name, err)
	}
	if affected > 1 {
		return fmt.Errorf("release lease %q: removed %d rows", name, affected)
	}
	return nil
}

// ListLeases returns the named leases held by owner.
func (s *Store) ListLeases(ctx context.Context, owner string) (map[string]time.Time, error) {
	if owner == "" {
		return nil, fmt.Errorf("lease owner is required")
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT name, expires_at FROM leases WHERE owner = ? AND expires_at > ?", owner, s.now().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("list leases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	leases := make(map[string]time.Time)
	for rows.Next() {
		var name string
		var expiresMs int64
		if err := rows.Scan(&name, &expiresMs); err != nil {
			return nil, fmt.Errorf("list leases: %w", err)
		}
		leases[name] = time.UnixMilli(expiresMs).UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list leases: %w", err)
	}
	return leases, nil
}

func validateLease(name, owner string, ttl time.Duration) error {
	if name == "" {
		return fmt.Errorf("lease name is required")
	}
	if owner == "" {
		return fmt.Errorf("lease owner is required")
	}
	if ttl < time.Millisecond {
		return fmt.Errorf("lease TTL must be at least %s", time.Millisecond)
	}
	return nil
}

func leaseDeadlineMilliseconds(now time.Time, ttl time.Duration) int64 {
	deadline := now.Add(ttl)
	milliseconds := deadline.UnixMilli()
	if time.UnixMilli(milliseconds).Before(deadline) {
		milliseconds++
	}
	return milliseconds
}
