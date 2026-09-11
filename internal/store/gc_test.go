package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/submission"
)

func runToTerminal(t *testing.T, s *store.Store, id string, terminal submission.State) {
	t.Helper()

	ctx := context.Background()
	for _, state := range []submission.State{
		submission.State(contract.StatusQueued),
		submission.State(contract.StatusRunning),
	} {
		if _, err := s.Transition(ctx, id, state, store.TransitionDetail{}); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}
	failure := contract.NewError(contract.CodeUpstreamExecutionFailed, "operation failed")
	if _, err := s.Transition(ctx, id, terminal, store.TransitionDetail{Error: &failure}); err != nil {
		t.Fatalf("transition to %s: %v", terminal, err)
	}
}

func TestGCSweepsPayloadThenTombstone(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	for _, state := range []submission.State{contract.StatusQueued, contract.StatusRunning} {
		if _, err := s.Transition(ctx, "sub-1", state, store.TransitionDetail{}); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}
	result := contract.Result{Content: []contract.ContentBlock{[]byte(`{"type":"text","text":"done"}`)}}
	if _, err := s.Complete(ctx, "sub-1", result); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// After payload retention the payload is cleared and the row becomes a
	// payload-free expired tombstone.
	clk.Advance(7*24*time.Hour + time.Minute)
	summary, err := s.GC(ctx)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if summary.Expired != 1 || summary.Deleted != 0 {
		t.Fatalf("GC summary = %+v, want one expired payload", summary)
	}
	got, err := s.GetSubmission(ctx, "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.Status != submission.State(contract.StatusExpired) {
		t.Fatalf("status = %s, want expired", got.Status)
	}
	if got.Result != nil || len(got.Events) != 0 || len(got.Arguments) != 0 || got.TaskID != "" || got.ErrorMessage != "" {
		t.Fatalf("tombstone still carries a payload: %+v", got.Result)
	}

	// After tombstone retention the row and its idempotency entry are gone,
	// freeing the client_request_id for a new canonical request.
	clk.Advance(30*24*time.Hour + time.Minute)
	summary, err = s.GC(ctx)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if summary.Deleted != 1 {
		t.Fatalf("GC summary = %+v, want one deleted tombstone", summary)
	}
	if _, err := s.GetSubmission(ctx, "sub-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("tombstone lookup = %v, want not found", err)
	}
	if _, err := s.CreateSubmission(ctx, testSubmission("sub-2", "req-1")); err != nil {
		t.Fatalf("reused client_request_id after tombstone: %v", err)
	}
}

func TestGCNeverTouchesNonTerminalSubmissions(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}

	clk.Advance(90 * 24 * time.Hour)
	summary, err := s.GC(ctx)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if summary.Expired != 0 || summary.Deleted != 0 {
		t.Fatalf("GC summary = %+v, want nothing swept", summary)
	}
	got, err := s.GetSubmission(ctx, "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.Status != submission.State(contract.StatusAccepted) {
		t.Fatalf("status = %s, want accepted", got.Status)
	}
}

func TestCheckIntegrity(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	if result, err := s.CheckIntegrity(context.Background()); err != nil {
		t.Fatalf("CheckIntegrity: %v", err)
	} else if result != "ok" {
		t.Fatalf("integrity = %q, want ok", result)
	}
}
