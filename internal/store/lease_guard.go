package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type leaseIdentity struct {
	name  string
	owner string
}

func (s *Store) requireLiveLease(ctx context.Context, tx *sql.Tx, lease leaseIdentity) error {
	var held int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM leases
		WHERE name = ? AND owner = ? AND expires_at > ?`,
		lease.name, lease.owner, s.now().UnixMilli(),
	).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseNotOwned
	}
	if err != nil {
		return fmt.Errorf("check lease %q: %w", lease.name, err)
	}
	return nil
}
