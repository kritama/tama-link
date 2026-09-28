package bootstrap

import (
	"errors"
	"fmt"
)

// Exit statuses returned through ExitError.
const (
	StatusSuccess = 0
	StatusFailed  = 1
	StatusUsage   = 2
)

// ExitError carries the command exit status for one bootstrap failure.
type ExitError struct {
	Status int
	Err    error
}

func (e *ExitError) Error() string {
	if e == nil || e.Err == nil {
		return "bootstrap failed"
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

func usageErr(err error) error {
	return &ExitError{Status: StatusUsage, Err: err}
}

func failErr(err error) error {
	return &ExitError{Status: StatusFailed, Err: err}
}

// ErrCancelled reports that the user stopped bootstrap before authorization.
var ErrCancelled = errors.New("login cancelled")

// ErrIncomplete reports that a durable bootstrap journal remains and can be
// resumed. It is not a successful login.
var ErrIncomplete = errors.New("bootstrap is incomplete and can be resumed")

// ErrBusy reports that another process holds the profile bootstrap lease.
var ErrBusy = errors.New("another bootstrap is already in progress for this profile")

func cancelled() error { return failErr(ErrCancelled) }

func incomplete(err error) error {
	if err == nil {
		return failErr(ErrIncomplete)
	}
	return failErr(fmt.Errorf("%w: %v", ErrIncomplete, err))
}
