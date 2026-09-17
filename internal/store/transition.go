package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/submission"
)

// TransitionDetail carries the optional facts of one state move.
type TransitionDetail struct {
	// TaskID is the current upstream task ID (may be stale after restarts).
	TaskID string
	// Error describes a terminal failure with its stable code and safe
	// message. It is ignored for non-terminal moves and completed results.
	Error *contract.Error
}

// Transition moves one submission to a new state, enforcing the state
// machine. Reaching a terminal state stamps completion and the payload and
// tombstone retention deadlines.
func (s *Store) Transition(ctx context.Context, id string, to submission.State, detail TransitionDetail) (*Submission, error) {
	return s.transition(ctx, id, to, detail, nil)
}

// TransitionLeased performs Transition only while owner holds a live lease.
// The lease check and state write share one transaction.
func (s *Store) TransitionLeased(
	ctx context.Context,
	id, leaseName, owner string,
	to submission.State,
	detail TransitionDetail,
) (*Submission, error) {
	return s.transition(ctx, id, to, detail, &leaseIdentity{name: leaseName, owner: owner})
}

func (s *Store) transition(
	ctx context.Context,
	id string,
	to submission.State,
	detail TransitionDetail,
	lease *leaseIdentity,
) (*Submission, error) {
	if !submission.Valid(to) {
		return nil, fmt.Errorf("unknown state %q", to)
	}
	if to == contract.StatusCompleted {
		return nil, errors.New("completed transitions require Complete")
	}
	if submission.Terminal(to) && detail.Error == nil {
		return nil, fmt.Errorf("terminal state %s requires a structured error", to)
	}
	if !submission.Terminal(to) && detail.Error != nil {
		return nil, fmt.Errorf("non-terminal state %s cannot carry an error", to)
	}
	if detail.Error != nil {
		if err := detail.Error.Validate(); err != nil {
			return nil, fmt.Errorf("terminal error: %w", err)
		}
	}

	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if lease != nil {
		if err := s.requireLiveLease(ctx, tx, *lease); err != nil {
			return nil, err
		}
	}

	sub, err := s.loadEncrypted(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	fromState := sub.Status
	if !submission.Allowed(fromState, to) {
		return nil, submission.IllegalTransition{From: fromState, To: to}
	}

	nowMs := s.now().UnixMilli()
	clauses := []string{"status = ?", "updated_at = ?"}
	args := []any{string(to), nowMs}

	if detail.TaskID != "" {
		clauses = append(clauses, "task_id = ?")
		args = append(args, detail.TaskID)
	}
	if detail.Error != nil {
		clauses = append(clauses, "error_code = ?", "error_message = ?", "error_retryable = ?")
		args = append(args, string(detail.Error.Code), detail.Error.Message, detail.Error.Retryable)
	}
	if submission.Terminal(to) {
		completedMs := nowMs
		clauses = append(clauses, "completed_at = ?", "payload_expires_at = ?", "tombstone_expires_at = ?")
		args = append(args,
			completedMs,
			completedMs+int64(sub.AcceptedLimits.PayloadRetention/time.Millisecond),
			completedMs+int64(sub.AcceptedLimits.TombstoneRetention/time.Millisecond),
		)
	}

	query := "UPDATE submissions SET " + strings.Join(clauses, ", ") + " WHERE submission_id = ? AND status = ?"
	args = append(args, id, string(fromState))
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update submission: %w", err)
	}
	if err := requireUpdated(result); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transition: %w", err)
	}

	return s.GetSubmission(ctx, id)
}

// Complete atomically moves a running submission to completed and captures its
// entire result. An oversized result atomically becomes a terminal failure;
// terminal result bytes are never truncated or written in a later transaction.
func (s *Store) Complete(ctx context.Context, id string, result contract.Result) (*Submission, error) {
	return s.complete(ctx, id, result, nil)
}

// CompleteLeased performs Complete only while owner holds a live lease. The
// lease check and terminal write share one transaction, preventing a stale
// worker from publishing a result after ownership moved to another process.
func (s *Store) CompleteLeased(
	ctx context.Context,
	id, leaseName, owner string,
	result contract.Result,
) (*Submission, error) {
	return s.complete(ctx, id, result, &leaseIdentity{name: leaseName, owner: owner})
}

func (s *Store) complete(ctx context.Context, id string, result contract.Result, lease *leaseIdentity) (*Submission, error) {
	if err := result.Validate(); err != nil {
		return nil, fmt.Errorf("validate result: %w", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	acceptedLimits, err := s.loadAcceptedLimits(ctx, id)
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > int64(acceptedLimits.ResultBytes) {
		failure := contract.NewError(contract.CodeResultTooLarge, "The upstream operation completed, but its result exceeded the configured storage limit.")
		sub, transitionErr := s.finish(ctx, id, contract.StatusFailed, nil, &failure, lease)
		if transitionErr != nil {
			return nil, transitionErr
		}
		return sub, fmt.Errorf("%w: %d bytes exceeds %d", ErrResultTooLarge, len(encoded), acceptedLimits.ResultBytes)
	}
	return s.finish(ctx, id, contract.StatusCompleted, encoded, nil, lease)
}

// FailLeased records a safe terminal failure while owner still holds the live
// submission lease.
func (s *Store) FailLeased(
	ctx context.Context,
	id, leaseName, owner string,
	failure contract.Error,
) (*Submission, error) {
	if err := failure.Validate(); err != nil {
		return nil, fmt.Errorf("terminal error: %w", err)
	}
	return s.finish(ctx, id, contract.StatusFailed, nil, &failure, &leaseIdentity{name: leaseName, owner: owner})
}

func (s *Store) finish(
	ctx context.Context,
	id string,
	to submission.State,
	encodedResult []byte,
	failure *contract.Error,
	lease *leaseIdentity,
) (*Submission, error) {
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin terminal capture: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if lease != nil {
		if err := s.requireLiveLease(ctx, tx, *lease); err != nil {
			return nil, err
		}
	}

	sub, err := s.loadEncrypted(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if !submission.Allowed(sub.Status, to) {
		return nil, submission.IllegalTransition{From: sub.Status, To: to}
	}

	var sealed []byte
	if encodedResult != nil {
		sealed, err = s.cipher.seal(encodedResult, id, "result")
		if err != nil {
			return nil, fmt.Errorf("seal result: %w", err)
		}
	}
	nowMs := s.now().UnixMilli()
	var errorCode, errorMessage any
	errorRetryable := false
	if failure != nil {
		errorCode, errorMessage = string(failure.Code), failure.Message
		errorRetryable = failure.Retryable
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE submissions
		SET status = ?, result_enc = ?, error_code = ?, error_message = ?, error_retryable = ?,
		    completed_at = ?, payload_expires_at = ?, tombstone_expires_at = ?, updated_at = ?
		WHERE submission_id = ? AND status = ?`,
		string(to), sealed, errorCode, errorMessage, errorRetryable,
		nowMs, nowMs+int64(sub.AcceptedLimits.PayloadRetention/time.Millisecond),
		nowMs+int64(sub.AcceptedLimits.TombstoneRetention/time.Millisecond), nowMs,
		id, string(sub.Status),
	)
	if err != nil {
		return nil, fmt.Errorf("store terminal result: %w", err)
	}
	if err := requireUpdated(result); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit terminal result: %w", err)
	}
	return s.GetSubmission(ctx, id)
}

func requireUpdated(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read updated row count: %w", err)
	}
	if affected != 1 {
		return ErrConcurrentUpdate
	}
	return nil
}

// loadEncrypted reads one submission row inside an open transaction without
// decrypting, so callers can re-seal the same blobs.
func (s *Store) loadEncrypted(ctx context.Context, tx *writeTx, id string) (*Submission, error) {
	row := tx.QueryRowContext(ctx, submissionQuery, id)
	sub, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return sub, err
}

const submissionQuery = `
		SELECT submission_id, client_request_id, tool, strategy, descriptor_digest,
		       args_hash, args_enc, task_id, status, sequence, events_enc, result_enc,
		       error_code, error_message, error_retryable, protocol_version, adapter_version,
		       response_bytes, result_bytes, event_bytes, max_events, events_bytes,
		       payload_retention_ms, tombstone_retention_ms,
		       created_at, updated_at, completed_at, payload_expires_at
		FROM submissions WHERE submission_id = ?`
