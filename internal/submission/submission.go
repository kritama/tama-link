// Package submission defines Tama Link's normalized submission state machine
// and progress event invariants.
//
// The state machine is the single source of truth for which state moves are
// legal. Terminal states are absorbing: once reached, a submission never
// moves again. Persistence, allocation, and enforcement live in the store;
// this package owns only the model.
package submission

import (
	"fmt"

	"github.com/kritama/tama-link/internal/contract"
)

// State is a normalized submission state.
type State = contract.Status

// FirstSequence is the first valid progress event sequence within one
// submission. Sequences are strictly increasing.
const FirstSequence int64 = 1

// Valid reports whether state is a known normalized state.
func Valid(state State) bool {
	switch state {
	case contract.StatusAccepted, contract.StatusQueued, contract.StatusRunning,
		contract.StatusCompleted, contract.StatusFailed, contract.StatusCancelled,
		contract.StatusExpired, contract.StatusOutcomeUnknown:
		return true
	default:
		return false
	}
}

// Terminal reports whether state is terminal and therefore absorbing.
func Terminal(state State) bool {
	switch state {
	case contract.StatusCompleted, contract.StatusFailed, contract.StatusCancelled,
		contract.StatusExpired, contract.StatusOutcomeUnknown:
		return true
	default:
		return false
	}
}

// Allowed reports whether moving from one state to another is legal. The
// only legal moves are accepted -> queued -> running and running to any
// terminal state.
func Allowed(from, to State) bool {
	if !Valid(from) || !Valid(to) {
		return false
	}
	switch from {
	case contract.StatusAccepted:
		return to == contract.StatusQueued
	case contract.StatusQueued:
		return to == contract.StatusRunning
	case contract.StatusRunning:
		return Terminal(to)
	default:
		return false
	}
}

// IllegalTransition describes an attempted illegal state move. It is an
// internal invariant violation, not a client error.
type IllegalTransition struct {
	From State
	To   State
}

func (e IllegalTransition) Error() string {
	return fmt.Sprintf("illegal state transition %s -> %s", e.From, e.To)
}

// SequenceError describes a progress event that violates the monotonic
// sequence invariant.
type SequenceError struct {
	Sequence int64
	Reason   string
}

func (e SequenceError) Error() string {
	return fmt.Sprintf("invalid event sequence %d: %s", e.Sequence, e.Reason)
}

// CheckSequence validates one event sequence against the last sequence
// observed for the same submission.
func CheckSequence(sequence, previous int64) error {
	if sequence < FirstSequence {
		return SequenceError{Sequence: sequence, Reason: "below first sequence"}
	}
	if sequence <= previous {
		return SequenceError{Sequence: sequence, Reason: "not strictly increasing"}
	}
	return nil
}
