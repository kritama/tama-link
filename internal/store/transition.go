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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var from string
	if err := tx.QueryRowContext(ctx, "SELECT status FROM submissions WHERE submission_id = ?", id).Scan(&from); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("read submission state: %w", err)
	}
	fromState := submission.State(from)
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

	query := "UPDATE submissions SET " + strings.Join(clauses, ", ") + " WHERE submission_id = ?"
	args = append(args, id)
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return nil, fmt.Errorf("update submission: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transition: %w", err)
	}

	moved := &Submission{ID: id, Status: to, UpdatedAt: time.UnixMilli(nowMs).UTC()}
	if submission.Terminal(to) {
		moved.CompletedAt = &moved.UpdatedAt
	}
	return moved, nil
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
	merged, err := mergeEvents(sub, events, s.limits)
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
	if _, err := tx.ExecContext(ctx,
		"UPDATE submissions SET events_enc = ?, sequence = ?, updated_at = ? WHERE submission_id = ?",
		sealed, merged[len(merged)-1].Sequence, nowMs, id,
	); err != nil {
		return nil, fmt.Errorf("store events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit events: %w", err)
	}

	sub.Events = merged
	sub.Sequence = int(merged[len(merged)-1].Sequence)
	return sub, nil
}

// CaptureResult stores the terminal result. The result must fit the profile
// result bound or the store refuses; it is never truncated.
func (s *Store) CaptureResult(ctx context.Context, id string, result contract.Result) (*Submission, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	if int64(len(encoded)) > int64(s.limits.ResultBytes) {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrResultTooLarge, len(encoded), s.limits.ResultBytes)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin result capture: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	sub, err := s.loadEncrypted(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if !submission.Terminal(sub.Status) {
		return nil, fmt.Errorf("capture result in state %s: only terminal states accept results", sub.Status)
	}

	sealed, err := s.cipher.seal(encoded)
	if err != nil {
		return nil, fmt.Errorf("seal result: %w", err)
	}
	nowMs := s.now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		"UPDATE submissions SET result_enc = ?, updated_at = ? WHERE submission_id = ?",
		sealed, nowMs, id,
	); err != nil {
		return nil, fmt.Errorf("store result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit result: %w", err)
	}

	sub.Result = &result
	return sub, nil
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
func mergeEvents(sub *Submission, events []contract.Event, lim limits.Limits) ([]contract.Event, error) {
	merged := append([]contract.Event(nil), sub.Events...)
	last := int64(0)
	if len(merged) > 0 {
		last = merged[len(merged)-1].Sequence
	}
	for _, event := range events {
		if event.SubmissionID != sub.ID {
			return nil, fmt.Errorf("event for submission %q stored under %q", event.SubmissionID, sub.ID)
		}
		if err := submission.CheckSequence(event.Sequence, last); err != nil {
			return nil, err
		}
		merged = append(merged, event)
		last = event.Sequence
	}
	return trimEvents(merged, lim), nil
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
