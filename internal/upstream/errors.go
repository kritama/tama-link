package upstream

import (
	"errors"
	"fmt"
)

// Kind classifies an upstream failure for boundary mapping.
type Kind int

// Failure kinds. Boundary layers map these to the stable contract error
// taxonomy; the kinds must stay mutually exclusive.
const (
	// KindTransport means the request never produced a decodable reply:
	// network failure, timeout, malformed body, or unsupported content type.
	KindTransport Kind = iota
	// KindHTTP means the endpoint answered with a status that carries no
	// usable JSON-RPC error object.
	KindHTTP
	// KindAuth means the endpoint rejected the credential (401/403).
	KindAuth
	// KindProtocol means the endpoint answered with a JSON-RPC protocol
	// error. Error.Code holds the JSON-RPC code.
	KindProtocol
	// KindTooLarge means a response or event exceeded its configured bound.
	KindTooLarge
)

// Error is a classified upstream failure. Messages are fixed and safe: they
// never carry request or response payloads, headers, or credentials.
type Error struct {
	// Kind is the failure class.
	Kind Kind
	// Code is the JSON-RPC error code for KindProtocol and the HTTP status
	// for KindHTTP. It is zero otherwise.
	Code int
	Err  error
}

// Error implements the error interface.
func (e *Error) Error() string {
	switch e.Kind {
	case KindTransport:
		return "upstream transport failure"
	case KindHTTP:
		return fmt.Sprintf("upstream HTTP status %d", e.Code)
	case KindAuth:
		return "upstream authorization rejected"
	case KindProtocol:
		return fmt.Sprintf("upstream protocol error %d", e.Code)
	case KindTooLarge:
		return "upstream response exceeds bound"
	default:
		return "upstream failure"
	}
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// newError builds a classified failure with a bounded cause. The cause is
// retained for programmatic inspection (for example context cancellation)
// but its message must not surface in diagnostics: Error.Error never prints
// it.
func newError(kind Kind, code int, cause error) *Error {
	if cause == nil {
		cause = errors.New("classified upstream failure")
	}
	return &Error{Kind: kind, Code: code, Err: cause}
}

// IsProtocol reports whether err is a KindProtocol failure.
func IsProtocol(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == KindProtocol
}

// IsAuth reports whether err is a KindAuth failure.
func IsAuth(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == KindAuth
}
