package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/submission"
)

// Submission is one durable local submission with its decrypted sensitive
// fields.
type Submission struct {
	ID               string
	ClientRequestID  string
	Tool             string
	Strategy         string
	DescriptorDigest string
	Arguments        json.RawMessage
	ArgsHash         string
	TaskID           string
	Status           submission.State
	Sequence         int64
	Events           []contract.Event
	Result           *contract.Result
	ErrorCode        string
	ErrorMessage     string
	ErrorRetryable   bool
	ProtocolVersion  string
	AdapterVersion   string
	AcceptedLimits   AcceptedLimits
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CompletedAt      *time.Time

	encArgs   []byte
	encEvents []byte
	encResult []byte
}

// NewSubmission is the input for creating one accepted submission.
// Arguments must be the validated canonical argument bytes; the idempotency
// hash covers the complete execution identity. Timestamps come from the store
// clock.
type NewSubmission struct {
	ID               string
	ClientRequestID  string
	Tool             string
	Strategy         string
	DescriptorDigest string
	Arguments        json.RawMessage
	ProtocolVersion  string
	AdapterVersion   string
}

// CreateSubmission inserts one accepted submission together with its
// idempotency index entry in a single transaction. A retry with the same
// client_request_id and the same canonical arguments returns the existing
// submission; a different argument hash is a conflict.
func (s *Store) CreateSubmission(ctx context.Context, sub NewSubmission) (*Submission, error) {
	if sub.ID == "" || sub.ClientRequestID == "" || sub.Tool == "" {
		return nil, errors.New("submission id, client request id, and tool are required")
	}
	// Canonicalize within the implementation ceiling before applying the
	// current profile limits. Existing idempotency records remain recoverable
	// when a profile later lowers its acceptance limits.
	arguments, err := canonicalArguments(sub.Arguments, limits.HardCeiling())
	if err != nil {
		return nil, err
	}
	sub.Arguments = arguments
	argsHash := hashInput(sub)

	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin idempotent insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Claim the client_request_id before inserting the submission row. This
	// makes an exact retry safe even when it reuses the original submission ID:
	// the durable idempotency record is reconciled before the submissions
	// primary key can reject the insert.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO idempotency (client_request_id, args_hash, submission_id, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(client_request_id) DO NOTHING`, sub.ClientRequestID, argsHash, sub.ID, now)
	if err != nil {
		return nil, fmt.Errorf("insert idempotency index: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("insert idempotency index: %w", err)
	}
	if affected == 0 {
		return s.reconcileIdempotentSubmission(ctx, tx, sub.ClientRequestID, argsHash)
	}
	if err := validateCanonicalArguments(arguments, s.limits); err != nil {
		return nil, err
	}
	encryptedArgs, err := s.cipher.seal(arguments, sub.ID, "arguments")
	if err != nil {
		return nil, fmt.Errorf("seal arguments: %w", err)
	}

	insert := `
		INSERT INTO submissions (
			submission_id, client_request_id, tool, strategy, descriptor_digest,
			args_hash, args_enc, task_id, status, sequence,
			protocol_version, adapter_version,
			response_bytes, result_bytes, event_bytes, max_events, events_bytes,
			payload_retention_ms, tombstone_retention_ms,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	acceptedLimits := acceptedLimitsFrom(s.limits)
	if _, err := tx.ExecContext(ctx, insert,
		sub.ID, sub.ClientRequestID, sub.Tool, sub.Strategy, sub.DescriptorDigest,
		argsHash, encryptedArgs,
		string(contract.StatusAccepted), sub.ProtocolVersion, sub.AdapterVersion,
		acceptedLimits.ResponseBytes, acceptedLimits.ResultBytes,
		acceptedLimits.EventBytes, acceptedLimits.MaxEvents, acceptedLimits.EventsBytes,
		acceptedLimits.PayloadRetention/time.Millisecond,
		acceptedLimits.TombstoneRetention/time.Millisecond,
		now, now,
	); err != nil {
		return nil, fmt.Errorf("insert submission: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit submission: %w", err)
	}

	created := &Submission{
		ID:               sub.ID,
		ClientRequestID:  sub.ClientRequestID,
		Tool:             sub.Tool,
		Strategy:         sub.Strategy,
		DescriptorDigest: sub.DescriptorDigest,
		Arguments:        sub.Arguments,
		ArgsHash:         argsHash,
		Status:           contract.StatusAccepted,
		ProtocolVersion:  sub.ProtocolVersion,
		AdapterVersion:   sub.AdapterVersion,
		AcceptedLimits:   acceptedLimits,
		CreatedAt:        time.UnixMilli(now).UTC(),
		UpdatedAt:        time.UnixMilli(now).UTC(),
	}
	return created, nil
}

func (s *Store) reconcileIdempotentSubmission(
	ctx context.Context,
	tx *sql.Tx,
	clientRequestID, argsHash string,
) (*Submission, error) {
	// End this transaction before reading through the pool. A concurrent
	// winner may still be committing the row referenced by the index.
	_ = tx.Rollback()
	var existingID, existingHash string
	if err := s.db.QueryRowContext(ctx,
		"SELECT submission_id, args_hash FROM idempotency WHERE client_request_id = ?",
		clientRequestID,
	).Scan(&existingID, &existingHash); err != nil {
		return nil, fmt.Errorf("read idempotency index: %w", err)
	}
	if existingHash != argsHash {
		return nil, fmt.Errorf("%w: client_request_id %q was used with different arguments",
			ErrIdempotencyConflict, clientRequestID)
	}
	return s.GetSubmission(ctx, existingID)
}

// hashInput hashes a length-delimited, versioned execution identity. Including
// the operation and pinned descriptor prevents the same client request ID from
// silently selecting different work with identical argument bytes.
func hashInput(sub NewSubmission) string {
	var input bytes.Buffer
	input.WriteString("tama-link/idempotency/v1\x00")
	for _, value := range [][]byte{
		[]byte(sub.Tool),
		[]byte(sub.Strategy),
		[]byte(sub.DescriptorDigest),
		sub.Arguments,
	} {
		_ = input.WriteByte(byte(len(value) >> 24))
		_ = input.WriteByte(byte(len(value) >> 16))
		_ = input.WriteByte(byte(len(value) >> 8))
		_ = input.WriteByte(byte(len(value)))
		input.Write(value)
	}
	sum := sha256.Sum256(input.Bytes())
	return hex.EncodeToString(sum[:])
}
