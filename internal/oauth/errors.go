package oauth

import "errors"

// Sentinels returned by this package. The contract boundary maps them as
// follows: ErrNoCredentials and ErrGrantInvalid become
// authentication_required; ErrBackendUnavailable becomes state_unavailable.
// ErrMetadata and ErrScopeMismatch fail the affected discovery, registration,
// or login operation; interactive login maps them to its sanitized CLI
// diagnostics.
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

	// ErrLeaseContention reports that another process currently owns the
	// refresh lease, so this process could not run a token exchange. The
	// credential is valid and the winner is refreshing it; the failure is
	// transient contention, not an authentication failure. Callers should
	// retry or defer the work instead of failing it as
	// authentication_required.
	ErrLeaseContention = errors.New("oauth: refresh lease is held by another process")

	// ErrScopeMismatch reports that the token endpoint or the registration
	// returned a scope set that differs from the scope set the profile
	// requested or the credential bound. Reduced, expanded, or malformed
	// grants fail closed; no credential from the response is persisted.
	ErrScopeMismatch = errors.New("oauth: returned scope set does not match the requested set")
)
