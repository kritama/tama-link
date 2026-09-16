package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

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
	decoded, err := s.cipher.open(enc, submissionID, "input-response")
	if err != nil {
		return nil, false, unreadableSubmissionPayload(submissionID, "input response "+requestID, err)
	}
	return decoded, true, nil
}

// SetInputResponse records the canonical response for one input-request ID.
// An existing record is left untouched; the caller must reconcile an existing
// record through GetInputResponse first.
func (s *Store) SetInputResponse(ctx context.Context, submissionID, requestID string, response json.RawMessage) error {
	if submissionID == "" || requestID == "" {
		return errors.New("submission id and request id are required")
	}
	if len(response) == 0 {
		return errors.New("input response is required")
	}
	sealed, err := s.cipher.seal(response, submissionID, "input-response")
	if err != nil {
		return fmt.Errorf("seal input response: %w", err)
	}
	if _, err := s.exec(ctx, `
		INSERT INTO input_responses (submission_id, request_id, response_enc, answered_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(submission_id, request_id) DO NOTHING`,
		submissionID, requestID, sealed, s.now().UnixMilli()); err != nil {
		return fmt.Errorf("record input response: %w", err)
	}
	return nil
}

// AttachTaskID records the owner-bound upstream task ID on a submission whose
// task ID is still empty. It is first-write-wins and idempotent for an
// identical value, so concurrent awaits and restarts never duplicate or
// overwrite the attachment.
func (s *Store) AttachTaskID(ctx context.Context, submissionID, taskID string) error {
	if submissionID == "" || taskID == "" {
		return errors.New("submission id and task id are required")
	}
	if _, err := s.exec(ctx, `
		UPDATE submissions SET task_id = ?, updated_at = ?
		WHERE submission_id = ? AND (task_id IS NULL OR task_id = '' OR task_id = ?)`,
		taskID, s.now().UnixMilli(), submissionID, taskID); err != nil {
		return fmt.Errorf("attach task id: %w", err)
	}
	return nil
}
