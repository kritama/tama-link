package oauth

import "errors"

// Sentinels returned by this package. The contract boundary maps them as
// follows: ErrNoCredentials and ErrGrantInvalid become
// authentication_required; ErrBackendUnavailable becomes state_unavailable.
var (
	// ErrNoCredentials reports that the profile has no usable refresh
	// credential. Interactive reauthorization (Phase 3 login) is required.
	ErrNoCredentials = errors.New("oauth: no stored credential")

	// ErrGrantInvalid reports that the authorization server rejected a
	// grant with invalid_grant. The credential is stale or revoked;
	// reauthorization is required. This error is never retried in a loop.
	ErrGrantInvalid = errors.New("oauth: grant rejected (invalid_grant)")

	// ErrBackendUnavailable reports that the secure credential backend
	// failed. Callers must fail closed; there is no plaintext fallback.
	ErrBackendUnavailable = errors.New("oauth: credential backend unavailable")

	// ErrMetadata reports that protected-resource or authorization-server
	// metadata is missing, malformed, or fails validation.
	ErrMetadata = errors.New("oauth: metadata validation failed")
)
