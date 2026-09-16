package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/kritama/tama-link/internal/contract"
)

// ReplaceTaskIDLeased replaces a stale session-scoped upstream task ID while
// the submission remains running. The lease check and correlation update share
// one transaction so a stale worker cannot overwrite a newer attachment.
func (s *Store) ReplaceTaskIDLeased(
	ctx context.Context,
	id, owner, taskID string,
) (*Submission, error) {
	if id == "" || owner == "" || taskID == "" {
		return nil, errors.New("submission id, owner, and task id are required")
	}
	leaseName := "submission/" + id
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin task reattachment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.requireLiveLease(ctx, tx, leaseIdentity{name: leaseName, owner: owner}); err != nil {
		return nil, err
	}
	sub, err := s.loadEncrypted(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if sub.Status != contract.StatusRunning {
		return nil, fmt.Errorf("submission %s is not running", id)
	}
	updated, err := tx.ExecContext(ctx, `
		UPDATE submissions SET task_id = ?, updated_at = ?
		WHERE submission_id = ? AND status = ?`,
		taskID, s.now().UnixMilli(), id, string(contract.StatusRunning),
	)
	if err != nil {
		return nil, fmt.Errorf("replace task id: %w", err)
	}
	if err := requireUpdated(updated); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit task reattachment: %w", err)
	}
	return s.GetSubmission(ctx, id)
}
