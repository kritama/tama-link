package store

import (
	"context"
	"fmt"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/submission"
)

// CaptureTerminalLeased records a terminal task state and its encrypted
// upstream evidence in one lease-checked transaction. The outstanding input
// map is cleared in that same write. Evidence is never truncated: a document
// past the accepted result bound is omitted and the stable error becomes
// result_too_large. A lost lease commits nothing.
func (s *Store) CaptureTerminalLeased(
	ctx context.Context,
	id, leaseName, owner string,
	to submission.State,
	failure contract.Error,
	evidence []byte,
) (*Submission, error) {
	if !submission.Terminal(to) || to == contract.StatusCompleted {
		return nil, fmt.Errorf("terminal evidence requires a non-completed terminal state")
	}
	if err := failure.Validate(); err != nil {
		return nil, fmt.Errorf("terminal error: %w", err)
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin terminal evidence: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.requireLiveLease(ctx, tx, leaseIdentity{name: leaseName, owner: owner}); err != nil {
		return nil, err
	}
	sub, err := s.loadEncrypted(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if submission.Terminal(sub.Status) {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit terminal evidence: %w", err)
		}
		return s.GetSubmission(ctx, id)
	}
	if !submission.Allowed(sub.Status, to) {
		return nil, submission.IllegalTransition{From: sub.Status, To: to}
	}
	var sealed []byte
	if len(evidence) > 0 {
		if int64(len(evidence)) > int64(sub.AcceptedLimits.ResultBytes) {
			failure = contract.NewError(contract.CodeResultTooLarge,
				"The upstream operation completed, but its result exceeded the configured storage limit.")
			to = contract.StatusFailed
		} else {
			sealed, err = s.cipher.seal(evidence, id, "terminal-evidence")
			if err != nil {
				return nil, fmt.Errorf("seal terminal evidence: %w", err)
			}
		}
	}
	nowMs := s.now().UnixMilli()
	res, err := tx.ExecContext(ctx, `
		UPDATE submissions
		SET status = ?, error_code = ?, error_message = ?, error_retryable = ?,
		    terminal_evidence_enc = ?, input_requests_enc = NULL,
		    completed_at = ?, payload_expires_at = ?, tombstone_expires_at = ?, updated_at = ?
		WHERE submission_id = ? AND status = ?`,
		string(to), string(failure.Code), failure.Message, failure.Retryable, sealed,
		nowMs, nowMs+int64(sub.AcceptedLimits.PayloadRetention/time.Millisecond),
		nowMs+int64(sub.AcceptedLimits.TombstoneRetention/time.Millisecond), nowMs,
		id, string(sub.Status),
	)
	if err != nil {
		return nil, fmt.Errorf("store terminal evidence: %w", err)
	}
	if err := requireUpdated(res); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit terminal evidence: %w", err)
	}
	return s.GetSubmission(ctx, id)
}
