package submission

import (
	"testing"

	"github.com/kritama/tama-link/internal/contract"
)

var allStates = []State{
	contract.StatusAccepted,
	contract.StatusQueued,
	contract.StatusRunning,
	contract.StatusCompleted,
	contract.StatusFailed,
	contract.StatusCancelled,
	contract.StatusExpired,
	contract.StatusOutcomeUnknown,
}

func TestValidState(t *testing.T) {
	t.Parallel()

	for _, state := range allStates {
		if !Valid(state) {
			t.Fatalf("Valid(%s) = false, want true", state)
		}
	}
	for _, state := range []State{"", "succeeded", "UNKNOWN", "accepted "} {
		if Valid(state) {
			t.Fatalf("Valid(%q) = true, want false", state)
		}
	}
}

func TestTerminal(t *testing.T) {
	t.Parallel()

	terminal := map[State]bool{
		contract.StatusCompleted:      true,
		contract.StatusFailed:         true,
		contract.StatusCancelled:      true,
		contract.StatusExpired:        true,
		contract.StatusOutcomeUnknown: true,
	}
	for _, state := range allStates {
		if got := Terminal(state); got != terminal[state] {
			t.Fatalf("Terminal(%s) = %v, want %v", state, got, terminal[state])
		}
	}
}

func TestTransitionMatrix(t *testing.T) {
	t.Parallel()

	wantAllowed := map[[2]State]bool{
		{contract.StatusAccepted, contract.StatusQueued}:        true,
		{contract.StatusQueued, contract.StatusRunning}:         true,
		{contract.StatusRunning, contract.StatusCompleted}:      true,
		{contract.StatusRunning, contract.StatusFailed}:         true,
		{contract.StatusRunning, contract.StatusCancelled}:      true,
		{contract.StatusRunning, contract.StatusExpired}:        true,
		{contract.StatusRunning, contract.StatusOutcomeUnknown}: true,
	}

	for _, from := range allStates {
		for _, to := range allStates {
			if got, want := Allowed(from, to), wantAllowed[[2]State{from, to}]; got != want {
				t.Errorf("Allowed(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTerminalStatesNeverReenterPending(t *testing.T) {
	t.Parallel()

	pending := []State{contract.StatusAccepted, contract.StatusQueued, contract.StatusRunning}
	for _, from := range []State{
		contract.StatusCompleted,
		contract.StatusFailed,
		contract.StatusCancelled,
		contract.StatusExpired,
		contract.StatusOutcomeUnknown,
	} {
		for _, to := range append(append([]State{}, pending...), from) {
			if Allowed(from, to) {
				t.Fatalf("Allowed(%s, %s) = true; terminal states must be absorbing", from, to)
			}
		}
	}
}

func TestIllegalTransitionError(t *testing.T) {
	t.Parallel()

	err := IllegalTransition{From: contract.StatusAccepted, To: contract.StatusCompleted}
	want := "illegal state transition accepted -> completed"
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestCheckSequence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sequence int64
		previous int64
		ok       bool
	}{
		{"first event", 1, 0, true},
		{"advances", 7, 6, true},
		{"below first", 0, 0, false},
		{"negative", -1, 0, false},
		{"duplicate", 5, 5, false},
		{"regresses", 4, 5, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := CheckSequence(test.sequence, test.previous)
			if (err == nil) != test.ok {
				t.Fatalf("CheckSequence(%d, %d) error = %v, want ok=%v", test.sequence, test.previous, err, test.ok)
			}
		})
	}
}

func TestSequenceError(t *testing.T) {
	t.Parallel()

	err := SequenceError{Sequence: 0, Reason: "below first sequence"}
	want := "invalid event sequence 0: below first sequence"
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}
