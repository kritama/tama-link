package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Input-response ciphertexts are bound to the submission and the input
// request: the AEAD additional data carries
// "input-response:<requestID>", so a ciphertext copied to another
// request row of the same submission fails authentication instead of
// handing the destination request the wrong durable response.

// GetInputResponse returns the canonical response previously recorded for one
// input-request ID, or found=false. The recorded response is the idempotency
// baseline: an exact replay is a no-op, a different response is a conflict.
func (s *Store) GetInputResponse(ctx context.Context, submissionID, requestID string) (json.RawMessage, bool, error) {
	if submissionID == "" || requestID == "" {
		return nil, false, errors.New("submission id and request id are required")
	}
	var enc []byte
	err := s.db.QueryRowContext(ctx,
		"SELECT response_enc FROM input_responses WHERE submission_id = ? AND request_id = ?",
		submissionID, requestID,
	).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read input response: %w", err)
	}
	decoded, err := s.cipher.open(enc, submissionID, "input-response:"+requestID)
	if err != nil {
		return nil, false, unreadableSubmissionPayload(submissionID, "input response "+requestID, err)
	}
	return decoded, true, nil
}

// ErrInputResponseConflict reports that a different response is already
// recorded for an input-request ID. The caller must reconcile through the
// winner (GetInputResponse); its own value was not stored.
var ErrInputResponseConflict = errors.New("a different response is already recorded for this input request")

// SetInputResponse records the canonical response for one input-request ID.
// Recording is atomic with conflict detection: when another process wins a
// concurrent insert with a different value, ErrInputResponseConflict is
// returned in the same operation, so the loser can reconcile against the
// durable winner instead of silently assuming its value won. An exact
// replay (the same normalized value) is a no-op.
func (s *Store) SetInputResponse(ctx context.Context, submissionID, requestID string, response json.RawMessage) error {
	if submissionID == "" || requestID == "" {
		return errors.New("submission id and request id are required")
	}
	if len(response) == 0 {
		return errors.New("input response is required")
	}
	sealed, err := s.cipher.seal(response, submissionID, "input-response:"+requestID)
	if err != nil {
		return fmt.Errorf("seal input response: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO input_responses (submission_id, request_id, response_enc, answered_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(submission_id, request_id) DO NOTHING`,
		submissionID, requestID, sealed, s.now().UnixMilli())
	if err != nil {
		return fmt.Errorf("record input response: %w", err)
	}
	if inserted, _ := res.RowsAffected(); inserted > 0 {
		return nil
	}
	// The row already existed: inspect the winner. An identical replay
	// succeeds; anything else is a conflict.
	stored, found, err := s.GetInputResponse(ctx, submissionID, requestID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("input response vanished between insert and read")
	}
	if string(stored) == string(response) {
		return nil
	}
	return ErrInputResponseConflict
}

// ErrTaskIDConflict reports that the submission already carries a different
// upstream task ID: the first attachment wins and the caller must reconcile
// against the durable owner.
var ErrTaskIDConflict = errors.New("submission already carries a different upstream task id")

// AttachTaskID records the owner-bound upstream task ID on a submission whose
// task ID is still empty. It is first-write-wins and idempotent for an
// identical value, so concurrent awaits and restarts never duplicate or
// overwrite the attachment. A zero-row update is an error, never a silent
// success: either the submission is missing or its task is already bound to
// a different ID.
func (s *Store) AttachTaskID(ctx context.Context, submissionID, taskID string) error {
	if submissionID == "" || taskID == "" {
		return errors.New("submission id and task id are required")
	}
	res, err := s.exec(ctx, `
		UPDATE submissions SET task_id = ?, updated_at = ?
		WHERE submission_id = ? AND (task_id IS NULL OR task_id = '' OR task_id = ?)`,
		taskID, s.now().UnixMilli(), submissionID, taskID)
	if err != nil {
		return fmt.Errorf("attach task id: %w", err)
	}
	if n, rerr := res.RowsAffected(); rerr == nil && n > 0 {
		return nil
	}
	var existing string
	qerr := s.db.QueryRowContext(ctx,
		"SELECT task_id FROM submissions WHERE submission_id = ?", submissionID).
		Scan(&existing)
	if errors.Is(qerr, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, submissionID)
	}
	if qerr != nil {
		return fmt.Errorf("attach task id: %w", qerr)
	}
	if existing != taskID {
		return fmt.Errorf("%w: submission %s carries task %q", ErrTaskIDConflict, submissionID, existing)
	}
	return nil
}
