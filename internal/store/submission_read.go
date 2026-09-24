package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/submission"
)

// GetSubmission returns one submission by ID with its decrypted arguments,
// events, and terminal result.
func (s *Store) GetSubmission(ctx context.Context, id string) (*Submission, error) {
	row := s.db.QueryRowContext(ctx, submissionQuery, id)

	sub, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if err := s.decryptSubmission(sub); err != nil {
		return nil, err
	}
	return sub, nil
}

func scanSubmission(row *sql.Row) (*Submission, error) {
	var sub Submission
	var argsEnc, eventsEnc, resultEnc, inputEnc, evidenceEnc []byte
	var taskID sql.NullString
	var taskCapabilities, taskUpdatedAt string
	var taskTTLMs, taskPollMs int64
	var status string
	var errorCode, errorMessage sql.NullString
	var errorRetryable int
	var completedAt, payloadExpiresAt sql.NullInt64
	var createdMs, updatedMs int64
	var payloadRetentionMs, tombstoneRetentionMs int64

	err := row.Scan(
		&sub.ID, &sub.ClientRequestID, &sub.Tool, &sub.Strategy, &sub.DescriptorDigest,
		&sub.ArgsHash, &argsEnc, &taskID, &status, &sub.Sequence, &eventsEnc, &resultEnc,
		&errorCode, &errorMessage, &errorRetryable, &sub.ProtocolVersion, &sub.AdapterVersion,
		&sub.AcceptedLimits.ResponseBytes, &sub.AcceptedLimits.ResultBytes,
		&sub.AcceptedLimits.EventBytes, &sub.AcceptedLimits.MaxEvents, &sub.AcceptedLimits.EventsBytes,
		&payloadRetentionMs, &tombstoneRetentionMs,
		&createdMs, &updatedMs, &completedAt, &payloadExpiresAt,
		&taskTTLMs, &taskPollMs, &taskCapabilities, &taskUpdatedAt, &inputEnc, &evidenceEnc,
	)
	if err != nil {
		return nil, fmt.Errorf("scan submission: %w", err)
	}
	sub.CreatedAt = time.UnixMilli(createdMs).UTC()
	sub.UpdatedAt = time.UnixMilli(updatedMs).UTC()
	sub.AcceptedLimits.PayloadRetention = time.Duration(payloadRetentionMs) * time.Millisecond
	sub.AcceptedLimits.TombstoneRetention = time.Duration(tombstoneRetentionMs) * time.Millisecond
	if err := sub.AcceptedLimits.validate(); err != nil {
		return nil, fmt.Errorf("%w: submission %s: %w", ErrStateUnavailable, sub.ID, err)
	}
	sub.Status = submission.State(status)
	if !submission.Valid(sub.Status) {
		return nil, fmt.Errorf("submission %s has unknown status %q", sub.ID, status)
	}
	sub.encArgs = argsEnc
	sub.encEvents = eventsEnc
	sub.encResult = resultEnc
	sub.TaskID = taskID.String
	sub.TaskTTLMs = taskTTLMs
	sub.TaskPollIntervalMs = taskPollMs
	sub.TaskCapabilities = taskCapabilities
	sub.TaskUpdatedAt = taskUpdatedAt
	sub.encInputRequests = inputEnc
	sub.encTerminalEvidence = evidenceEnc
	if errorCode.Valid {
		sub.ErrorCode = errorCode.String
	}
	if errorMessage.Valid {
		sub.ErrorMessage = errorMessage.String
	}
	sub.ErrorRetryable = errorRetryable != 0
	if completedAt.Valid {
		completed := time.UnixMilli(completedAt.Int64).UTC()
		sub.CompletedAt = &completed
	}
	if payloadExpiresAt.Valid {
		expires := time.UnixMilli(payloadExpiresAt.Int64).UTC()
		sub.PayloadExpiresAt = &expires
	}
	return &sub, nil
}

// decryptSubmission fills Arguments, Events, and Result from the sealed
// blobs. Empty blobs stay absent.
func (s *Store) decryptSubmission(sub *Submission) error {
	if len(sub.encArgs) > 0 {
		args, err := s.cipher.open(sub.encArgs, sub.ID, "arguments")
		if err != nil {
			return unreadableSubmissionPayload(sub.ID, "arguments", err)
		}
		sub.Arguments = args
	}
	if len(sub.encEvents) > 0 {
		events, err := s.cipher.open(sub.encEvents, sub.ID, "events")
		if err != nil {
			return unreadableSubmissionPayload(sub.ID, "events", err)
		}
		decoded := []contract.Event{}
		if err := json.Unmarshal(events, &decoded); err != nil {
			return unreadableSubmissionPayload(sub.ID, "events", err)
		}
		sub.Events = decoded
	}
	if len(sub.encInputRequests) > 0 {
		raw, err := s.cipher.open(sub.encInputRequests, sub.ID, "input-requests")
		if err != nil {
			return unreadableSubmissionPayload(sub.ID, "input requests", err)
		}
		sub.InputRequests = raw
	}
	if len(sub.encTerminalEvidence) > 0 {
		raw, err := s.cipher.open(sub.encTerminalEvidence, sub.ID, "terminal-evidence")
		if err != nil {
			return unreadableSubmissionPayload(sub.ID, "terminal evidence", err)
		}
		sub.TerminalEvidence = raw
	}
	if len(sub.encResult) > 0 {
		result, err := s.cipher.open(sub.encResult, sub.ID, "result")
		if err != nil {
			return unreadableSubmissionPayload(sub.ID, "result", err)
		}
		decoded := contract.Result{}
		if err := json.Unmarshal(result, &decoded); err != nil {
			return unreadableSubmissionPayload(sub.ID, "result", err)
		}
		sub.Result = &decoded
	}
	return nil
}

func unreadableSubmissionPayload(id, field string, err error) error {
	return fmt.Errorf("%w: submission %s %s: %w", ErrStateUnavailable, id, field, err)
}
