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
	ID                 string
	ClientRequestID    string
	Tool               string
	Strategy           string
	DescriptorDigest   string
	Arguments          json.RawMessage
	ArgsHash           string
	TaskID             string
	TaskTTLMs          int64
	TaskPollIntervalMs int64
	TaskCapabilities   string
	TaskUpdatedAt      string
	InputRequests      json.RawMessage
	// TerminalEvidence is the validated upstream failure or cancellation
	// document. Await never returns it.
	TerminalEvidence json.RawMessage
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
	// PayloadExpiresAt bounds how long the terminal payload is retrievable
	// after completion. Reads past this deadline return submission_expired.
	PayloadExpiresAt *time.Time

	encArgs             []byte
	encEvents           []byte
	encResult           []byte
	encInputRequests    []byte
	encTerminalEvidence []byte
}

// NewSubmission is the input for creating one accepted submission.
// Arguments must be the validated canonical upstream argument bytes;
// RequestArguments must be the canonical client-visible request bytes the
// idempotency identity covers. Timestamps come from the store clock.
type NewSubmission struct {
	ID               string
	ClientRequestID  string
	Tool             string
	Strategy         string
	DescriptorDigest string
	// Arguments is the validated canonical upstream argument bytes stored
	// on the submission after bindings were applied.
	Arguments json.RawMessage
	// RequestArguments is the canonical client-visible request — the raw
	// client arguments plus the client-owned correlation values bindings
	// may map — that the idempotency hash covers. It is captured before
	// current schema and binding state applies, so a retry reconciles to
	// the original submission even when the profile was reconciled.
	RequestArguments json.RawMessage
	// IdentityCandidates are alternate client-visible identities this
	// request may correspond to, under the binding set in force at
	// acceptance elsewhere. They are used only when another process
	// claims the key concurrently and this insert loses: when any
	// candidate hash matches the winner's, the winner's row is returned
	// as a replay instead of a conflict. The stored identity remains
	// RequestArguments.
	IdentityCandidates []json.RawMessage
	ProtocolVersion    string
	AdapterVersion     string
}

// CreateSubmission inserts one accepted submission together with its
// idempotency index entry in a single transaction. A retry with the same
// client_request_id and the same canonical arguments returns the existing
// submission with created=false; a different argument hash is a conflict.
// Callers use created to distinguish a fresh acceptance (which still needs
// its acceptance event and dispatch) from a replay of an existing row.
func (s *Store) CreateSubmission(ctx context.Context, sub NewSubmission) (*Submission, bool, error) {
	if sub.ID == "" || sub.ClientRequestID == "" || sub.Tool == "" || len(sub.RequestArguments) == 0 {
		return nil, false, errors.New("submission id, client request id, tool, and request arguments are required")
	}
	// Canonicalize within the implementation ceiling before applying the
	// current profile limits. Existing idempotency records remain recoverable
	// when a profile later lowers its acceptance limits.
	arguments, err := canonicalArguments(sub.Arguments, limits.HardCeiling())
	if err != nil {
		return nil, false, err
	}
	sub.Arguments = arguments
	requestArguments, err := canonicalArguments(sub.RequestArguments, limits.HardCeiling())
	if err != nil {
		return nil, false, err
	}
	sub.RequestArguments = requestArguments
	argsHash := hashInput(sub)
	// The insert-race reconciliation below must match every identity
	// shape the concurrent winner's acceptance could have stored: the
	// winner's binding set need not be this process's.
	candidateHashes := make([]string, 0, len(sub.IdentityCandidates)+1)
	for _, raw := range sub.IdentityCandidates {
		candidate, err := canonicalArguments(raw, limits.HardCeiling())
		if err != nil {
			continue
		}
		candidateHashes = append(candidateHashes,
			hashInput(NewSubmission{Tool: sub.Tool, RequestArguments: candidate}))
	}
	candidateHashes = append(candidateHashes, argsHash)

	now := s.now().UnixMilli()
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin idempotent insert: %w", err)
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
		return nil, false, fmt.Errorf("insert idempotency index: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("insert idempotency index: %w", err)
	}
	if affected == 0 {
		return s.reconcileIdempotentSubmission(ctx, tx, sub.ClientRequestID, candidateHashes)
	}
	if err := validateCanonicalArguments(arguments, s.limits); err != nil {
		return nil, false, err
	}
	encryptedArgs, err := s.cipher.seal(arguments, sub.ID, "arguments")
	if err != nil {
		return nil, false, fmt.Errorf("seal arguments: %w", err)
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
		return nil, false, fmt.Errorf("insert submission: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit submission: %w", err)
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
	return created, true, nil
}

// ReconcileIdempotentSubmission returns the durable submission previously
// accepted for clientRequestID, or found=false when the key is unclaimed.
// The lookup is read-only: it never claims the key, mutates the row, or
// appends events. A retry reconciles through this method from the
// client-visible request before any current schema, binding, readiness, or
// strategy state applies, so recovery of an already accepted request never
// depends on them. The caller supplies the candidate client-visible
// identities the retry may correspond to: the stored identity was shaped
// by the binding set in force at acceptance, which the retry cannot know,
// so recovery matches any candidate and never the current descriptor. A
// key reused with a different client-visible request is
// ErrIdempotencyConflict. A candidate outside the canonical hard ceilings
// can never match an accepted request and is skipped.
func (s *Store) ReconcileIdempotentSubmission(
	ctx context.Context,
	clientRequestID, tool string,
	identities []json.RawMessage,
) (*Submission, bool, error) {
	if clientRequestID == "" {
		return nil, false, errors.New("client request id is required")
	}
	candidateHashes := make([]string, 0, len(identities))
	for _, raw := range identities {
		arguments, err := canonicalArguments(raw, limits.HardCeiling())
		if err != nil {
			continue
		}
		candidateHashes = append(candidateHashes,
			hashInput(NewSubmission{Tool: tool, RequestArguments: arguments}))
	}
	var existingID, existingHash string
	err := s.db.QueryRowContext(ctx,
		"SELECT submission_id, args_hash FROM idempotency WHERE client_request_id = ?",
		clientRequestID,
	).Scan(&existingID, &existingHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read idempotency index: %w", err)
	}
	for _, candidate := range candidateHashes {
		if candidate == existingHash {
			existing, err := s.GetSubmission(ctx, existingID)
			if err != nil {
				return nil, false, err
			}
			return existing, true, nil
		}
	}
	return nil, false, fmt.Errorf("%w: client_request_id %q was used with different arguments",
		ErrIdempotencyConflict, clientRequestID)
}

func (s *Store) reconcileIdempotentSubmission(
	ctx context.Context,
	tx *writeTx,
	clientRequestID string,
	argsHashes []string,
) (*Submission, bool, error) {
	// End this transaction before reading through the pool. A concurrent
	// winner may still be committing the row referenced by the index.
	_ = tx.Rollback()
	var existingID, existingHash string
	if err := s.db.QueryRowContext(ctx,
		"SELECT submission_id, args_hash FROM idempotency WHERE client_request_id = ?",
		clientRequestID,
	).Scan(&existingID, &existingHash); err != nil {
		return nil, false, fmt.Errorf("read idempotency index: %w", err)
	}
	for _, argsHash := range argsHashes {
		if existingHash == argsHash {
			existing, err := s.GetSubmission(ctx, existingID)
			return existing, false, err
		}
	}
	return nil, false, fmt.Errorf("%w: client_request_id %q was used with different arguments",
		ErrIdempotencyConflict, clientRequestID)
}

// hashInput hashes a length-delimited, versioned client request identity:
// the tool name and the canonical client-visible request bytes. Live
// profile state — the execution strategy, the pinned descriptor digest,
// and the bound upstream arguments — is deliberately excluded, so a retry
// after a profile reconciliation reconciles to the original submission
// instead of reporting a conflict. The executor rechecks the accepted
// digest against the live catalog before any upstream call.
func hashInput(sub NewSubmission) string {
	var input bytes.Buffer
	input.WriteString("tama-link/idempotency/v1\x00")
	for _, value := range [][]byte{
		[]byte(sub.Tool),
		sub.RequestArguments,
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
