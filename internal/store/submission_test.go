package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/submission"
)

func TestCreateAndGetRoundTrip(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	created, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1"))
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if created.Status != submission.State(contract.StatusAccepted) {
		t.Fatalf("status = %s, want accepted", created.Status)
	}
	if created.CreatedAt != clk.Now() {
		t.Fatalf("created at = %s, want the store clock", created.CreatedAt)
	}

	got, err := s.GetSubmission(ctx, "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if string(got.Arguments) != `{"message":"hi"}` {
		t.Fatalf("arguments = %s", got.Arguments)
	}
	if got.Tool != "message" || got.DescriptorDigest != "sha256:abc" {
		t.Fatalf("descriptor fields = %+v", got)
	}
	if got.ProtocolVersion != "2025-11-25" || got.AdapterVersion != "tama014/1" {
		t.Fatalf("protocol fields = %+v", got)
	}
}

func TestCreateIdempotentHit(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	first, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1"))
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	retry, err := s.CreateSubmission(ctx, testSubmission("sub-2", "req-1"))
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if retry.ID != first.ID {
		t.Fatalf("retry returned %q, want the original %q", retry.ID, first.ID)
	}
	if _, err := s.GetSubmission(ctx, "sub-2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second id lookup = %v, want not found", err)
	}
}

func TestCreateIdempotentConflict(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	conflict := testSubmission("sub-2", "req-1")
	conflict.Arguments = []byte(`{"message":"changed"}`)
	if _, err := s.CreateSubmission(ctx, conflict); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("conflicting retry = %v, want ErrIdempotencyConflict", err)
	}
}

func TestCreateIdempotencyIncludesOperationIdentity(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()
	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	conflict := testSubmission("sub-2", "req-1")
	conflict.Tool = "review"
	if _, err := s.CreateSubmission(ctx, conflict); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("different tool retry = %v, want ErrIdempotencyConflict", err)
	}
}

func TestCreateValidatesInput(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	noID := testSubmission("", "req-1")
	if _, err := s.CreateSubmission(ctx, noID); err == nil {
		t.Fatal("missing submission id accepted, want error")
	}

	noClient := testSubmission("sub-1", "")
	if _, err := s.CreateSubmission(ctx, noClient); err == nil {
		t.Fatal("missing client request id accepted, want error")
	}

	noTool := testSubmission("sub-1", "req-1")
	noTool.Tool = ""
	if _, err := s.CreateSubmission(ctx, noTool); err == nil {
		t.Fatal("missing tool accepted, want error")
	}

	invalidJSON := testSubmission("sub-1", "req-1")
	invalidJSON.Arguments = []byte(`{"message":`)
	if _, err := s.CreateSubmission(ctx, invalidJSON); err == nil {
		t.Fatal("invalid JSON arguments accepted, want error")
	}
}

func TestCreateEnforcesArgumentBounds(t *testing.T) {
	t.Parallel()

	lim := limits.Default()
	lim.ArgumentsBytes = 16
	lim.ArgumentDepth = 2
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(context.Background(), path, newMemKeys(), store.Config{Limits: lim})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	tooLarge := testSubmission("sub-large", "req-large")
	tooLarge.Arguments = []byte(`{"message":"this is too large"}`)
	if _, err := s.CreateSubmission(context.Background(), tooLarge); err == nil {
		t.Fatal("oversized arguments accepted")
	}
	tooDeep := testSubmission("sub-deep", "req-deep")
	tooDeep.Arguments = []byte(`{"a":{"b":{"c":1}}}`)
	if _, err := s.CreateSubmission(context.Background(), tooDeep); err == nil {
		t.Fatal("overly deep arguments accepted")
	}
}

func TestGetMissingSubmission(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	if _, err := s.GetSubmission(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSubmission = %v, want ErrNotFound", err)
	}
}

func TestTransitionLifecycle(t *testing.T) {
	t.Parallel()

	clk := newClock()
	s, _ := openTestStore(t, newMemKeys(), clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}

	if _, err := s.Transition(ctx, "sub-1", submission.State(contract.StatusQueued), store.TransitionDetail{}); err != nil {
		t.Fatalf("accepted -> queued: %v", err)
	}
	if _, err := s.Transition(ctx, "sub-1", submission.State(contract.StatusRunning), store.TransitionDetail{TaskID: "task-1"}); err != nil {
		t.Fatalf("queued -> running: %v", err)
	}

	terminal, err := s.Complete(ctx, "sub-1", contract.Result{Content: []contract.ContentBlock{[]byte(`{"type":"text","text":"done"}`)}})
	if err != nil {
		t.Fatalf("running -> completed: %v", err)
	}
	if terminal.CompletedAt == nil || *terminal.CompletedAt != clk.Now() {
		t.Fatalf("terminal completion = %+v, want the store clock", terminal.CompletedAt)
	}

	got, err := s.GetSubmission(ctx, "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.TaskID != "task-1" {
		t.Fatalf("task id = %q, want task-1", got.TaskID)
	}
	if got.CompletedAt == nil {
		t.Fatal("completed at not persisted")
	}
}

func TestTransitionRejectsIllegalMoves(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}

	tests := []struct {
		name string
		to   submission.State
	}{
		{"accepted to running", submission.State(contract.StatusRunning)},
		{"accepted to completed", submission.State(contract.StatusCompleted)},
		{"unknown state", submission.State("bogus")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if _, err := s.Transition(ctx, "sub-1", test.to, store.TransitionDetail{}); err == nil {
				t.Fatalf("illegal transition %s accepted", test.name)
			}
		})
	}
}

func TestTerminalStatesAreAbsorbing(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	for _, state := range []submission.State{
		submission.State(contract.StatusQueued),
		submission.State(contract.StatusRunning),
	} {
		if _, err := s.Transition(ctx, "sub-1", state, store.TransitionDetail{}); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}
	failure := contract.NewError(contract.CodeUpstreamExecutionFailed, "operation failed")
	if _, err := s.Transition(ctx, "sub-1", contract.StatusFailed, store.TransitionDetail{Error: &failure}); err != nil {
		t.Fatalf("transition to failed: %v", err)
	}

	if _, err := s.Transition(ctx, "sub-1", submission.State(contract.StatusCancelled), store.TransitionDetail{}); err == nil {
		t.Fatal("failed -> cancelled accepted, want rejection")
	}
	if _, err := s.Transition(ctx, "sub-1", submission.State(contract.StatusCompleted), store.TransitionDetail{}); err == nil {
		t.Fatal("failed -> completed accepted, want rejection")
	}
}

func TestTransitionStoresTerminalError(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	for _, state := range []submission.State{
		submission.State(contract.StatusQueued),
		submission.State(contract.StatusRunning),
	} {
		if _, err := s.Transition(ctx, "sub-1", state, store.TransitionDetail{}); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}

	failure := contract.NewError(contract.CodeUpstreamUnavailable, "upstream is unreachable")
	if _, err := s.Transition(ctx, "sub-1", submission.State(contract.StatusFailed), store.TransitionDetail{Error: &failure}); err != nil {
		t.Fatalf("transition to failed: %v", err)
	}

	got, err := s.GetSubmission(ctx, "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.ErrorCode != string(contract.CodeUpstreamUnavailable) || got.ErrorMessage != "upstream is unreachable" {
		t.Fatalf("terminal error = %q / %q", got.ErrorCode, got.ErrorMessage)
	}
}
