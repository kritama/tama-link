package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// VerifyStateKey decrypts one existing ciphertext, when the database has
// one, and discards the plaintext. An empty database succeeds once Open has
// accepted the key. A wrong key fails closed without a replacement.
func (s *Store) VerifyStateKey(ctx context.Context) error {
	checks := []struct {
		query string
		kind  string
	}{
		{`SELECT submission_id, args_enc FROM submissions WHERE args_enc IS NOT NULL LIMIT 1`, "arguments"},
		{`SELECT submission_id, events_enc FROM submissions WHERE events_enc IS NOT NULL LIMIT 1`, "events"},
		{`SELECT submission_id, result_enc FROM submissions WHERE result_enc IS NOT NULL LIMIT 1`, "result"},
		{`SELECT submission_id, input_requests_enc FROM submissions WHERE input_requests_enc IS NOT NULL LIMIT 1`, "input-requests"},
		{`SELECT submission_id, terminal_evidence_enc FROM submissions WHERE terminal_evidence_enc IS NOT NULL LIMIT 1`, "terminal-evidence"},
	}
	for _, check := range checks {
		if err := s.verifySealedColumn(ctx, check.query, check.kind); err != nil {
			return err
		}
	}
	return s.verifyInputResponse(ctx)
}

func (s *Store) verifySealedColumn(ctx context.Context, query, kind string) error {
	var id string
	var blob []byte
	err := s.db.QueryRowContext(ctx, query).Scan(&id, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: read encrypted state: %w", ErrStateUnavailable, err)
	}
	if _, err := s.cipher.open(blob, id, kind); err != nil {
		return fmt.Errorf("%w: state key cannot decrypt existing data", ErrStateUnavailable)
	}
	return nil
}

func (s *Store) verifyInputResponse(ctx context.Context) error {
	var id, requestID string
	var blob []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT submission_id, request_id, response_enc
		FROM input_responses
		WHERE response_enc IS NOT NULL
		LIMIT 1`).Scan(&id, &requestID, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: read encrypted state: %w", ErrStateUnavailable, err)
	}
	if _, err := s.cipher.open(blob, id, "input-response:"+requestID); err != nil {
		return fmt.Errorf("%w: state key cannot decrypt existing data", ErrStateUnavailable)
	}
	return nil
}
