package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

func TestCreateExactRetryReusesSubmission(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()
	input := testSubmission("sub-1", "req-1")
	first, err := s.CreateSubmission(ctx, input)
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	retry, err := s.CreateSubmission(ctx, input)
	if err != nil {
		t.Fatalf("exact idempotent retry: %v", err)
	}
	if retry.ID != first.ID || retry.ClientRequestID != first.ClientRequestID {
		t.Fatalf("retry = %+v, want original %+v", retry, first)
	}
}

func TestCreateCanonicalizesArgumentsBeforeIdempotency(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()
	first := testSubmission("sub-1", "req-1")
	first.Arguments = []byte(`{ "z": 9007199254740993, "nested": {"b": 2, "a": 1} }`)
	created, err := s.CreateSubmission(ctx, first)
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	want := `{"nested":{"a":1,"b":2},"z":9007199254740993}`
	if string(created.Arguments) != want {
		t.Fatalf("canonical arguments = %s, want %s", created.Arguments, want)
	}

	retry := testSubmission("sub-2", "req-1")
	retry.Arguments = []byte("{\n\t\"nested\": {\"a\":1, \"b\":2}, \"z\":9007199254740993\n}")
	got, err := s.CreateSubmission(ctx, retry)
	if err != nil {
		t.Fatalf("equivalent idempotent retry: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("retry returned %q, want original %q", got.ID, created.ID)
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

func TestCreateIdempotencyIgnoresRuntimeVersions(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()
	first, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1"))
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	retry := testSubmission("sub-2", "req-1")
	retry.ProtocolVersion = "2026-07-28"
	retry.AdapterVersion = "tama014/2"
	got, err := s.CreateSubmission(ctx, retry)
	if err != nil {
		t.Fatalf("retry after runtime version change: %v", err)
	}
	if got.ID != first.ID {
		t.Fatalf("retry returned %q, want original %q", got.ID, first.ID)
	}
	if got.ProtocolVersion != first.ProtocolVersion || got.AdapterVersion != first.AdapterVersion {
		t.Fatalf("retry changed persisted recovery versions: %+v", got)
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

	duplicateKey := testSubmission("sub-duplicate", "req-duplicate")
	duplicateKey.Arguments = []byte(`{"message":"first","message":"second"}`)
	if _, err := s.CreateSubmission(ctx, duplicateKey); err == nil {
		t.Fatal("duplicate argument key accepted, want error")
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

func TestReplaceRunningTaskIDRequiresLiveLease(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()
	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if _, err := s.Transition(ctx, "sub-1", contract.StatusQueued, store.TransitionDetail{}); err != nil {
		t.Fatalf("queue submission: %v", err)
	}
	if _, err := s.Transition(ctx, "sub-1", contract.StatusRunning, store.TransitionDetail{TaskID: "stale-task"}); err != nil {
		t.Fatalf("start submission: %v", err)
	}
	leaseName := "submission/sub-1"
	if owned, err := s.ClaimLease(ctx, leaseName, "owner-a", time.Minute); err != nil || !owned {
		t.Fatalf("ClaimLease = %v, %v", owned, err)
	}
	if _, err := s.ReplaceTaskIDLeased(ctx, "sub-1", "owner-b", "wrong-task"); !errors.Is(err, store.ErrLeaseNotOwned) {
		t.Fatalf("replace without lease = %v, want ErrLeaseNotOwned", err)
	}
	got, err := s.ReplaceTaskIDLeased(ctx, "sub-1", "owner-a", "fresh-task")
	if err != nil {
		t.Fatalf("ReplaceTaskIDLeased: %v", err)
	}
	if got.Status != contract.StatusRunning || got.TaskID != "fresh-task" {
		t.Fatalf("reattached submission = status %s, task %q", got.Status, got.TaskID)
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
	if !got.ErrorRetryable {
		t.Fatal("terminal retryability was not persisted")
	}
}

func TestConcurrentTransitionHasSingleWinner(t *testing.T) {
	keys := newMemKeys()
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := store.Open(context.Background(), path, keys, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := store.Open(context.Background(), path, keys, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := first.CreateSubmission(context.Background(), testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, s := range []*store.Store{first, second} {
		go func(s *store.Store) {
			ready.Done()
			<-start
			_, err := s.Transition(context.Background(), "sub-1", contract.StatusQueued, store.TransitionDetail{})
			errs <- err
		}(s)
	}
	ready.Wait()
	close(start)
	successes := 0
	for range 2 {
		if err := <-errs; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful transitions = %d, want 1", successes)
	}
	got, err := first.GetSubmission(context.Background(), "sub-1")
	if err != nil || got.Status != contract.StatusQueued {
		t.Fatalf("final submission = %+v, %v", got, err)
	}
}
