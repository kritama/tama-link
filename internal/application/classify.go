package application

import (
	"errors"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/upstream"
)

// BoundaryError carries one stable contract error through interfaces that
// return plain errors. The worker records the carried error verbatim on the
// submission through its ContractError method.
type BoundaryError struct {
	E contract.Error
}

// Error renders only the stable code, never upstream payloads.
func (b *BoundaryError) Error() string { return string(b.E.Code) }

// ContractError returns the stable contract error carried by this failure.
func (b *BoundaryError) ContractError() *contract.Error { return &b.E }

// classify maps an adapter or upstream failure to the stable contract
// taxonomy at the application boundary. Messages are fixed and safe: they
// never carry upstream payloads, headers, or credentials.
func classify(err error) *contract.Error {
	var be *BoundaryError
	if errors.As(err, &be) {
		return be.ContractError()
	}
	switch {
	case errors.Is(err, tama2026.ErrAuthenticationRequired), upstream.IsAuth(err):
		return stable(contract.CodeAuthenticationRequired,
			"Tama requires authentication. Complete authorization for the selected profile, then retry.")
	case errors.Is(err, tama2026.ErrProtocolMismatch):
		return stable(contract.CodeProtocolMismatch,
			"The upstream endpoint no longer matches the protocol pinned by the selected profile.")
	case errors.Is(err, tama2026.ErrCatalogMismatch):
		return stable(contract.CodeOperationContractMismatch,
			"The upstream endpoint no longer matches the operation catalog pinned by the selected profile.")
	case errors.Is(err, tama2026.ErrUnexpectedTaskResult):
		return stable(contract.CodeOperationContractMismatch,
			"The upstream returned a task result for a pinned synchronous operation.")
	case errors.Is(err, tama2026.ErrOperationNotAllowed):
		return stable(contract.CodeOperationNotAllowed,
			"The selected profile does not allow this operation to execute.")
	case errors.Is(err, tama2026.ErrStateUnavailable):
		return stable(contract.CodeStateUnavailable,
			"The secure credential backend is unavailable for this profile.")
	default:
		return stable(contract.CodeUpstreamUnavailable,
			"The upstream endpoint is currently unavailable.")
	}
}

func stable(code contract.Code, message string) *contract.Error {
	e := contract.NewError(code, message)
	return &e
}
