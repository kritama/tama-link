package contract

// Code is a stable client-facing error code.
type Code string

// Stable error taxonomy.
const (
	CodeInvalidRequest            Code = "invalid_request"
	CodeOperationNotAllowed       Code = "operation_not_allowed"
	CodeOperationContractMismatch Code = "operation_contract_mismatch"
	CodeIdempotencyConflict       Code = "idempotency_conflict"
	CodeSubmissionNotFound        Code = "submission_not_found"
	CodeSubmissionExpired         Code = "submission_expired"
	CodeAuthenticationRequired    Code = "authentication_required"
	CodeAuthorizationFailed       Code = "authorization_failed"
	CodeProtocolMismatch          Code = "protocol_mismatch"
	CodeUpstreamUnavailable       Code = "upstream_unavailable"
	CodeUpstreamExecutionFailed   Code = "upstream_execution_failed"
	CodeOutcomeUnknown            Code = "outcome_unknown"
	CodeResultTooLarge            Code = "result_too_large"
	CodeStateUnavailable          Code = "state_unavailable"
	CodeInternal                  Code = "internal"
	CodeNotImplemented            Code = "not_implemented"
)

// Error is the stable structured error returned by Tama Link tools. Message
// must be a safe user-facing summary; it must never contain secret material.
type Error struct {
	Code      Code   `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// NewError returns an Error for code with the taxonomy's default
// retryability. Callers may override Retryable when the specific condition
// differs from the default.
func NewError(code Code, message string) Error {
	return Error{Code: code, Message: message, Retryable: defaultRetryable(code)}
}

func defaultRetryable(code Code) bool {
	switch code {
	case CodeUpstreamUnavailable:
		return true
	default:
		return false
	}
}
