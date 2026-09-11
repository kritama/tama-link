package store

import (
	"context"
	"fmt"
	"time"
)

// ClaimLease atomically acquires the named lease for owner. A lease is
// acquired when it does not exist, is expired, or is already held by owner.
// It reports whether owner holds the lease after the call.
func (s *Store) ClaimLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	nowMs := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO leases (name, owner, expires_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET owner = excluded.owner, expires_at = excluded.expires_at
		WHERE leases.expires_at <= ? OR leases.owner = excluded.owner`, name, owner, nowMs+ttl.Milliseconds(), nowMs)
	if err != nil {
		return false, fmt.Errorf("claim lease %q: %w", name, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim lease %q: %w", name, err)
	}
	return affected == 1, nil
}

// RenewLease extends the named lease. It reports whether owner holds the
// lease after the call.
func (s *Store) RenewLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	nowMs := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		"UPDATE leases SET expires_at = ? WHERE name = ? AND owner = ?",
		nowMs+ttl.Milliseconds(), name, owner)
	if err != nil {
		return false, fmt.Errorf("renew lease %q: %w", name, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("renew lease %q: %w", name, err)
	}
	return affected == 1, nil
}

// ReleaseLease removes the named lease when owner holds it.
func (s *Store) ReleaseLease(ctx context.Context, name, owner string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM leases WHERE name = ? AND owner = ?", name, owner)
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
	rows, err := s.db.QueryContext(ctx, "SELECT name, expires_at FROM leases WHERE owner = ?", owner)
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
