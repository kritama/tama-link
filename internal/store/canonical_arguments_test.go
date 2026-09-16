package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kritama/tama-link/internal/store"
)

func TestEmptyArrayArgumentsRemainDistinctFromNull(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	input := testSubmission("sub-1", "req-1")
	input.Arguments = []byte(`{"items":[]}`)
	input.RequestArguments = input.Arguments
	created, _, err := s.CreateSubmission(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if got, want := string(created.Arguments), `{"items":[]}`; got != want {
		t.Fatalf("canonical arguments = %s, want %s", got, want)
	}

	conflict := testSubmission("sub-2", "req-1")
	conflict.Arguments = []byte(`{"items":null}`)
	conflict.RequestArguments = conflict.Arguments
	if _, _, err := s.CreateSubmission(context.Background(), conflict); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("null retry = %v, want ErrIdempotencyConflict", err)
	}
}
