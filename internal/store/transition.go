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
	"github.com/kritama/tama-link/internal/limits"
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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
		clauses = append(clauses, "error_code = ?", "error_message = ?")
		args = append(args, string(detail.Error.Code), detail.Error.Message)
	}
	if submission.Terminal(to) {
		completedMs := nowMs
		clauses = append(clauses, "completed_at = ?", "payload_expires_at = ?", "tombstone_expires_at = ?")
		args = append(args,
			completedMs,
			completedMs+int64(s.limits.PayloadRetention/time.Millisecond),
			completedMs+int64(s.limits.TombstoneRetention/time.Millisecond),
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

// AppendEvents appends normalized progress events, validating the strictly
// increasing sequence invariant. Retention keeps at most MaxEvents events
// and EventsBytes total; overflow drops the oldest events first.
func (s *Store) AppendEvents(ctx context.Context, id string, events []contract.Event) (*Submission, error) {
	if len(events) == 0 {
		return s.GetSubmission(ctx, id)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin event append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	sub, err := s.loadEncrypted(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := s.decryptSubmission(sub); err != nil {
		return nil, err
	}
	if submission.Terminal(sub.Status) {
		return nil, fmt.Errorf("append events in terminal state %s", sub.Status)
	}
	merged, last, err := mergeEvents(sub, events, s.limits)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode events: %w", err)
	}
	sealed, err := s.cipher.seal(encoded)
	if err != nil {
		return nil, fmt.Errorf("seal events: %w", err)
	}
	nowMs := s.now().UnixMilli()
	result, err := tx.ExecContext(ctx,
		"UPDATE submissions SET events_enc = ?, sequence = ?, updated_at = ? WHERE submission_id = ? AND status = ? AND sequence = ?",
		sealed, last, nowMs, id, string(sub.Status), sub.Sequence,
	)
	if err != nil {
		return nil, fmt.Errorf("store events: %w", err)
	}
	if err := requireUpdated(result); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit events: %w", err)
	}

	sub.Events = merged
	sub.Sequence = last
	return sub, nil
}

// Complete atomically moves a running submission to completed and captures its
// entire result. An oversized result atomically becomes a terminal failure;
// terminal result bytes are never truncated or written in a later transaction.
func (s *Store) Complete(ctx context.Context, id string, result contract.Result) (*Submission, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	if int64(len(encoded)) > int64(s.limits.ResultBytes) {
		failure := contract.NewError(contract.CodeResultTooLarge, "The upstream operation completed, but its result exceeded the configured storage limit.")
		sub, transitionErr := s.finish(ctx, id, contract.StatusFailed, nil, &failure)
		if transitionErr != nil {
			return nil, transitionErr
		}
		return sub, fmt.Errorf("%w: %d bytes exceeds %d", ErrResultTooLarge, len(encoded), s.limits.ResultBytes)
	}
	return s.finish(ctx, id, contract.StatusCompleted, encoded, nil)
}

func (s *Store) finish(
	ctx context.Context,
	id string,
	to submission.State,
	encodedResult []byte,
	failure *contract.Error,
) (*Submission, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin terminal capture: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	sub, err := s.loadEncrypted(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if !submission.Allowed(sub.Status, to) {
		return nil, submission.IllegalTransition{From: sub.Status, To: to}
	}

	var sealed []byte
	if encodedResult != nil {
		sealed, err = s.cipher.seal(encodedResult)
		if err != nil {
			return nil, fmt.Errorf("seal result: %w", err)
		}
	}
	nowMs := s.now().UnixMilli()
	var errorCode, errorMessage any
	if failure != nil {
		errorCode, errorMessage = string(failure.Code), failure.Message
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE submissions
		SET status = ?, result_enc = ?, error_code = ?, error_message = ?,
		    completed_at = ?, payload_expires_at = ?, tombstone_expires_at = ?, updated_at = ?
		WHERE submission_id = ? AND status = ?`,
		string(to), sealed, errorCode, errorMessage,
		nowMs, nowMs+int64(s.limits.PayloadRetention/time.Millisecond),
		nowMs+int64(s.limits.TombstoneRetention/time.Millisecond), nowMs,
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
func (s *Store) loadEncrypted(ctx context.Context, tx *sql.Tx, id string) (*Submission, error) {
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
		       error_code, error_message, protocol_version, adapter_version,
		       created_at, updated_at, completed_at
		FROM submissions WHERE submission_id = ?`

// mergeEvents appends new events after validating the sequence invariant
// and trims the merged list to the retention bounds.
func mergeEvents(sub *Submission, events []contract.Event, lim limits.Limits) ([]contract.Event, int64, error) {
	merged := append([]contract.Event(nil), sub.Events...)
	last := sub.Sequence
	for _, event := range events {
		if event.SubmissionID != sub.ID {
			return nil, last, fmt.Errorf("event for submission %q stored under %q", event.SubmissionID, sub.ID)
		}
		if err := submission.CheckSequence(event.Sequence, last); err != nil {
			return nil, last, err
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, last, fmt.Errorf("encode event %d: %w", event.Sequence, err)
		}
		if int64(len(encoded)) > int64(lim.EventBytes) || int64(len(encoded)) > int64(lim.EventsBytes) {
			return nil, last, fmt.Errorf("event %d exceeds retention bounds", event.Sequence)
		}
		merged = append(merged, event)
		last = event.Sequence
	}
	return trimEvents(merged, lim), last, nil
}

// trimEvents drops the oldest events until the count and byte bounds hold.
func trimEvents(events []contract.Event, lim limits.Limits) []contract.Event {
	for len(events) > lim.MaxEvents {
		events = events[1:]
	}
	for len(events) > 0 && encodedEventsSize(events) > int64(lim.EventsBytes) {
		events = events[1:]
	}
	return events
}

func encodedEventsSize(events []contract.Event) int64 {
	var total int64
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			continue
		}
		total += int64(len(encoded))
	}
	return total
}
