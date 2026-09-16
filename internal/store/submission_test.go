package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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

	created, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1"))
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

	first, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1"))
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	retry, _, err := s.CreateSubmission(ctx, testSubmission("sub-2", "req-1"))
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
	first, _, err := s.CreateSubmission(ctx, input)
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	retry, _, err := s.CreateSubmission(ctx, input)
	if err != nil {
		t.Fatalf("exact idempotent retry: %v", err)
	}
	if retry.ID != first.ID || retry.ClientRequestID != first.ClientRequestID {
		t.Fatalf("retry = %+v, want original %+v", retry, first)
	}
}

func TestCreateRetrySurvivesLoweredArgumentLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		arguments []byte
		lower     func(*limits.Limits)
	}{
		{
			name:      "bytes",
			arguments: []byte(`{"message":"larger than sixteen bytes"}`),
			lower: func(l *limits.Limits) {
				l.ArgumentsBytes = 16
			},
		},
		{
			name:      "depth",
			arguments: []byte(`{"a":{"b":{"c":1}}}`),
			lower: func(l *limits.Limits) {
				l.ArgumentDepth = 2
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			keys := newMemKeys()
			path := filepath.Join(t.TempDir(), "state.db")
			initial, err := store.Open(ctx, path, keys, store.Config{Limits: limits.Default()})
			if err != nil {
				t.Fatalf("Open initial store: %v", err)
			}
			input := testSubmission("sub-1", "req-1")
			input.Arguments = test.arguments
			created, _, err := initial.CreateSubmission(ctx, input)
			if err != nil {
				t.Fatalf("CreateSubmission: %v", err)
			}
			if err := initial.Close(); err != nil {
				t.Fatalf("close initial store: %v", err)
			}

			lowered := limits.Default()
			test.lower(&lowered)
			reopened, err := store.Open(ctx, path, keys, store.Config{Limits: lowered})
			if err != nil {
				t.Fatalf("Open with lowered limits: %v", err)
			}
			t.Cleanup(func() { _ = reopened.Close() })

			retry := input
			retry.ID = "sub-2"
			got, _, err := reopened.CreateSubmission(ctx, retry)
			if err != nil {
				t.Fatalf("retry after lowering limits: %v", err)
			}
			if got.ID != created.ID {
				t.Fatalf("retry returned %q, want original %q", got.ID, created.ID)
			}
			conflict := input
			conflict.ID = "sub-conflict"
			conflict.Tool = "other"
			if _, _, err := reopened.CreateSubmission(ctx, conflict); !errors.Is(err, store.ErrIdempotencyConflict) {
				t.Fatalf("conflicting retry = %v, want ErrIdempotencyConflict", err)
			}

			fresh := input
			fresh.ID = "sub-3"
			fresh.ClientRequestID = "req-2"
			if _, _, err := reopened.CreateSubmission(ctx, fresh); err == nil {
				t.Fatal("new submission exceeding lowered limits was accepted")
			}
		})
	}
}

func TestCreateCanonicalizesArgumentsBeforeIdempotency(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()
	first := testSubmission("sub-1", "req-1")
	first.Arguments = []byte(`{ "z": 9007199254740993, "nested": {"b": 2, "a": 1} }`)
	created, _, err := s.CreateSubmission(ctx, first)
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	want := `{"nested":{"a":1,"b":2},"z":9007199254740993}`
	if string(created.Arguments) != want {
		t.Fatalf("canonical arguments = %s, want %s", created.Arguments, want)
	}

	retry := testSubmission("sub-2", "req-1")
	retry.Arguments = []byte("{\n\t\"nested\": {\"a\":1, \"b\":2}, \"z\":9007199254740993\n}")
	got, _, err := s.CreateSubmission(ctx, retry)
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

	if _, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	conflict := testSubmission("sub-2", "req-1")
	conflict.Arguments = []byte(`{"message":"changed"}`)
	conflict.RequestArguments = conflict.Arguments
	if _, _, err := s.CreateSubmission(ctx, conflict); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("conflicting retry = %v, want ErrIdempotencyConflict", err)
	}
}

func TestCreateIdempotencyIncludesOperationIdentity(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()
	if _, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	conflict := testSubmission("sub-2", "req-1")
	conflict.Tool = "review"
	if _, _, err := s.CreateSubmission(ctx, conflict); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("different tool retry = %v, want ErrIdempotencyConflict", err)
	}
}

func TestCreateIdempotencyIgnoresRuntimeVersions(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()
	first, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1"))
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	retry := testSubmission("sub-2", "req-1")
	retry.ProtocolVersion = "2026-07-28"
	retry.AdapterVersion = "tama014/2"
	got, _, err := s.CreateSubmission(ctx, retry)
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

func TestCreateIdempotencyIgnoresDescriptorAndStrategy(t *testing.T) {
	t.Parallel()

	// The strategy and the pinned descriptor are live profile state: a
	// retry after a profile reconciliation must reconcile to the original
	// submission instead of reporting a conflict solely because those
	// fields changed.
	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()
	first, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1"))
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	retry := testSubmission("sub-2", "req-1")
	retry.Strategy = "local_replayable"
	retry.DescriptorDigest = "sha256:reconciled"
	got, created, err := s.CreateSubmission(ctx, retry)
	if err != nil {
		t.Fatalf("retry after profile reconciliation: %v", err)
	}
	if created || got.ID != first.ID {
		t.Fatalf("retry returned %q created=%v, want original %q", got.ID, created, first.ID)
	}
	// The original row keeps its accepted descriptor and strategy.
	if got.DescriptorDigest != first.DescriptorDigest || got.Strategy != first.Strategy {
		t.Fatalf("reconciled row changed profile fields: %+v", got)
	}
}

func TestReconcileIdempotentSubmission(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()
	if _, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}

	// An exact replay reconciles to the original row without claiming or
	// mutating anything.
	got, found, err := s.ReconcileIdempotentSubmission(ctx, "req-1", "message",
		[]json.RawMessage{[]byte(`{"message":"hi"}`)})
	if err != nil || !found {
		t.Fatalf("reconcile = %v found=%v, want the original submission", err, found)
	}
	if got.ID != "sub-1" {
		t.Fatalf("reconcile returned %q, want sub-1", got.ID)
	}

	// An unclaimed key is a miss, not an error, and stays free.
	if _, found, err := s.ReconcileIdempotentSubmission(ctx, "req-2", "message",
		[]json.RawMessage{[]byte(`{"message":"hi"}`)}); err != nil || found {
		t.Fatalf("unclaimed reconcile = found=%v err=%v, want miss", found, err)
	}

	// A key reused with a different client-visible request is a conflict.
	if _, _, err := s.ReconcileIdempotentSubmission(ctx, "req-1", "message",
		[]json.RawMessage{[]byte(`{"message":"different"}`)}); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("conflicting reconcile = %v, want ErrIdempotencyConflict", err)
	}

	// Recovery matches any candidate identity: the stored base shape still
	// reconciles when the retry's full shape (thread ID included) is tried
	// alongside it, because the binding set in force at acceptance is not
	// the one in force now.
	if _, found, err := s.ReconcileIdempotentSubmission(ctx, "req-1", "message",
		[]json.RawMessage{
			[]byte(`{"arguments":{"message":"hi"},"thread_id":"t-1"}`),
			[]byte(`{"message":"hi"}`),
		}); err != nil || !found {
		t.Fatalf("candidate reconcile = found=%v err=%v, want the original submission", found, err)
	}

	// ...but a retry that offers only the thread-shaped identity does not
	// silently absorb a change to the correlation value.
	if _, _, err := s.ReconcileIdempotentSubmission(ctx, "req-1", "message",
		[]json.RawMessage{[]byte(`{"arguments":{"message":"hi"},"thread_id":"t-2"}`)}); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("thread-shape-only reconcile = %v, want ErrIdempotencyConflict", err)
	}
}

// TestCreateSubmissionInsertRaceReconcilesCandidates pins the cross-
// process race: two processes straddling a profile reconciliation submit
// the same client_request_id concurrently. Both preliminary lookups miss;
// the winner stores the identity shape from its binding set, and the
// loser's acceptance shape differs — but its candidate identities still
// match the winner's row, so the loser recovers a replay instead of
// reporting a conflict for an otherwise exact request.
func TestCreateSubmissionInsertRaceReconcilesCandidates(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	a, path := openTestStore(t, keys, clk)
	b, err := store.Open(context.Background(), path, keys, store.Config{
		Limits: limits.Default(),
		Now:    clk.Now,
	})
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()

	threaded := json.RawMessage(`{"arguments":{"message":"hi"},"thread_id":"t-1"}`)
	base := json.RawMessage(`{"arguments":{"message":"hi"}}`)

	// The winner's acceptance stored the threaded identity shape.
	win, created, err := a.CreateSubmission(ctx, testSubmissionWithIdentity("sub-w", "race-1", threaded))
	if err != nil || !created {
		t.Fatalf("winner CreateSubmission: created=%v err=%v", created, err)
	}

	// The loser's process could not map the source: its acceptance shape
	// is the base one. Without candidates the differing shape conflicts...
	if _, _, err := b.CreateSubmission(ctx, testSubmissionWithIdentity("sub-l", "race-1", base)); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("no-candidate loser = %v, want ErrIdempotencyConflict", err)
	}

	// ...while the candidate identities reconcile to the winner's row.
	loser := testSubmissionWithIdentity("sub-l", "race-1", base)
	loser.IdentityCandidates = []json.RawMessage{threaded, base}
	lose, created, err := b.CreateSubmission(ctx, loser)
	if err != nil {
		t.Fatalf("loser CreateSubmission = %v, want a replay through the candidates", err)
	}
	if created {
		t.Fatalf("loser CreateSubmission = created, want the winner's row as a replay")
	}
	if lose.ID != win.ID {
		t.Fatalf("loser reconciled to %q, want the winner %q", lose.ID, win.ID)
	}

	// Candidates that match no stored shape still conflict.
	stranger := testSubmissionWithIdentity("sub-x", "race-1", base)
	stranger.IdentityCandidates = []json.RawMessage{
		json.RawMessage(`{"arguments":{"message":"other"},"thread_id":"t-1"}`), base,
	}
	if _, _, err := b.CreateSubmission(ctx, stranger); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("unmatched candidates = %v, want ErrIdempotencyConflict", err)
	}
}

// testSubmissionWithIdentity builds one submission whose idempotency
// identity is the given client-visible request bytes.
func testSubmissionWithIdentity(id, clientRequestID string, identity json.RawMessage) store.NewSubmission {
	ns := testSubmission(id, clientRequestID)
	ns.RequestArguments = identity
	return ns
}

// TestCreateReconcilesClientVisibleIdentity pins that the idempotency
// identity is the client-visible request, never the bound upstream
// arguments: when a reconciled profile binds the same client request
// differently, an exact retry still reconciles to the original submission,
// while a different client-visible request conflicts.
func TestCreateReconcilesClientVisibleIdentity(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	ctx := context.Background()
	first := testSubmission("sub-1", "req-1")
	first.Arguments = []byte(`{"message":"hi","client_meta":{"client_request_id":"req-1"}}`)
	if _, _, err := s.CreateSubmission(ctx, first); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}

	// The retry's client-visible request is unchanged, but the reconciled
	// profile binds it to different upstream arguments.
	retry := testSubmission("sub-2", "req-1")
	retry.Arguments = []byte(`{"message":"hi","client_meta":{"client_request_id":"req-1","thread":"reconciled"}}`)
	got, created, err := s.CreateSubmission(ctx, retry)
	if err != nil {
		t.Fatalf("retry after binding reconciliation: %v", err)
	}
	if created || got.ID != "sub-1" {
		t.Fatalf("retry returned %q created=%v, want original sub-1", got.ID, created)
	}

	// A different client-visible request conflicts even when the bound
	// upstream arguments would coincide.
	conflict := testSubmission("sub-3", "req-1")
	conflict.RequestArguments = []byte(`{"message":"different"}`)
	conflict.Arguments = first.Arguments
	if _, _, err := s.CreateSubmission(ctx, conflict); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("different client-visible request = %v, want ErrIdempotencyConflict", err)
	}
}

func TestCreateValidatesInput(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	noID := testSubmission("", "req-1")
	if _, _, err := s.CreateSubmission(ctx, noID); err == nil {
		t.Fatal("missing submission id accepted, want error")
	}

	noClient := testSubmission("sub-1", "")
	if _, _, err := s.CreateSubmission(ctx, noClient); err == nil {
		t.Fatal("missing client request id accepted, want error")
	}

	noTool := testSubmission("sub-1", "req-1")
	noTool.Tool = ""
	if _, _, err := s.CreateSubmission(ctx, noTool); err == nil {
		t.Fatal("missing tool accepted, want error")
	}

	invalidJSON := testSubmission("sub-1", "req-1")
	invalidJSON.Arguments = []byte(`{"message":`)
	if _, _, err := s.CreateSubmission(ctx, invalidJSON); err == nil {
		t.Fatal("invalid JSON arguments accepted, want error")
	}

	duplicateKey := testSubmission("sub-duplicate", "req-duplicate")
	duplicateKey.Arguments = []byte(`{"message":"first","message":"second"}`)
	if _, _, err := s.CreateSubmission(ctx, duplicateKey); err == nil {
		t.Fatal("duplicate argument key accepted, want error")
	}

	excessiveDepth := testSubmission("sub-excessive-depth", "req-excessive-depth")
	excessiveDepth.Arguments = []byte(strings.Repeat("[", 1_000) + "0" + strings.Repeat("]", 1_000))
	if _, _, err := s.CreateSubmission(ctx, excessiveDepth); err == nil || !strings.Contains(err.Error(), "arguments depth") {
		t.Fatalf("excessively nested arguments = %v, want bounded depth error", err)
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
	if _, _, err := s.CreateSubmission(context.Background(), tooLarge); err == nil {
		t.Fatal("oversized arguments accepted")
	}
	tooDeep := testSubmission("sub-deep", "req-deep")
	tooDeep.Arguments = []byte(`{"a":{"b":{"c":1}}}`)
	if _, _, err := s.CreateSubmission(context.Background(), tooDeep); err == nil {
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

	if _, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
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
	if _, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
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

	if _, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
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

	if _, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
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

	if _, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
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
	if _, _, err := first.CreateSubmission(context.Background(), testSubmission("sub-1", "req-1")); err != nil {
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
