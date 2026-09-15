package tama2026

import "errors"

// Sentinels returned at the adapter boundary. The contract boundary maps
// them to stable codes: ErrAuthenticationRequired -> authentication_required,
// ErrProtocolMismatch -> protocol_mismatch, ErrCatalogMismatch ->
// operation_contract_mismatch, ErrOperationNotAllowed ->
// operation_not_allowed, ErrStateUnavailable -> state_unavailable.
var (
	// ErrAuthenticationRequired reports that the upstream rejected the
	// credential or no credential is available. Interactive reauthorization
	// is required; the error is never retried in a loop.
	ErrAuthenticationRequired = errors.New("tama2026: authentication required")

	// ErrProtocolMismatch reports that the discovered protocol version,
	// server capabilities, or an observed result shape violates the pinned
	// 2026-07-28 contract.
	ErrProtocolMismatch = errors.New("tama2026: protocol mismatch")

	// ErrCatalogMismatch reports that the live catalog, instructions, or a
	// pinned descriptor drifted from the profile.
	ErrCatalogMismatch = errors.New("tama2026: catalog mismatch")

	// ErrOperationNotAllowed reports that the pinned strategy does not
	// permit execution through this adapter.
	ErrOperationNotAllowed = errors.New("tama2026: operation not allowed")

	// ErrStateUnavailable reports that the secure credential backend
	// failed. There is no plaintext fallback.
	ErrStateUnavailable = errors.New("tama2026: credential backend unavailable")
)
