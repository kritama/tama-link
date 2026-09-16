package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
)

// awaitTerminal polls await until the submission is terminal or the test
// deadline passes. It returns the terminal output.
func awaitTerminal(t *testing.T, svc *Service, id string) contract.AwaitOutput {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out, appErr := svc.Await(context.Background(), contract.AwaitInput{SubmissionID: id, TimeoutMS: 200})
		if appErr != nil {
			t.Fatalf("await: %s", appErr.Message)
		}
		if out.Terminal {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("submission %s did not reach a terminal state", id)
	return contract.AwaitOutput{}
}

func TestAwaitUnknownSubmission(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)

	_, appErr := svc.Await(context.Background(), contract.AwaitInput{SubmissionID: "nope"})
	if appErr == nil || appErr.Code != contract.CodeSubmissionNotFound {
		t.Fatalf("error = %+v, want submission_not_found", appErr)
	}
}

func TestAwaitValidation(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)
	id := submitStatus(t, svc, "await-val")

	cases := []struct {
		name string
		in   contract.AwaitInput
	}{
		{name: "bad cursor", in: contract.AwaitInput{SubmissionID: id, Cursor: "not-a-number"}},
		{name: "negative timeout", in: contract.AwaitInput{SubmissionID: id, TimeoutMS: -1}},
		{name: "timeout above max", in: contract.AwaitInput{SubmissionID: id, TimeoutMS: 10 * 3600 * 1000}},
		{name: "input responses on non-input state",
			in: contract.AwaitInput{SubmissionID: id, InputResponses: map[string]json.RawMessage{"r": json.RawMessage(`{}`)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, appErr := svc.Await(context.Background(), tc.in)
			if appErr == nil || appErr.Code != contract.CodeInvalidRequest {
				t.Fatalf("error = %+v, want invalid_request", appErr)
			}
		})
	}
}

func TestAwaitCursorReplay(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)
	id := submitStatus(t, svc, "cursor-1")

	out := awaitTerminal(t, svc, id)
	if out.Cursor == "" {
		t.Fatal("terminal await returned no cursor")
	}
	if len(out.Events) == 0 {
		t.Fatal("terminal await returned no events")
	}

	// Replaying from the final cursor returns the terminal state with no
	// new events.
	replay, appErr := svc.Await(context.Background(), contract.AwaitInput{SubmissionID: id, Cursor: out.Cursor})
	if appErr != nil {
		t.Fatalf("replay: %s", appErr.Message)
	}
	if len(replay.Events) != 0 {
		t.Fatalf("replay returned %d new events, want 0", len(replay.Events))
	}
	if !replay.Terminal || replay.Status != contract.StatusCompleted {
		t.Fatalf("replay status = %s terminal=%v", replay.Status, replay.Terminal)
	}
}

func TestAwaitFailureMapping(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	f.fail.Store(true)
	svc, _, _ := fixtureApp(t, f)

	id := submitStatus(t, svc, "fail-1")
	out := awaitTerminal(t, svc, id)
	if out.Status != contract.StatusFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.Error == nil {
		t.Fatal("failed await returned no error")
	}
	if out.Error.Retryable {
		t.Fatalf("terminal error must not be retryable: %+v", out.Error)
	}
}

func TestAwaitCancellationStopsWait(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)
	id := submitStatus(t, svc, "cancel-1")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The fixture completes quickly, but a long timeout forces the await to
	// wait; cancellation must end the local wait promptly.
	_, appErr := svc.Await(ctx, contract.AwaitInput{SubmissionID: id, TimeoutMS: 30000})
	if appErr != nil {
		t.Fatalf("await after cancel: %s", appErr.Message)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("await waited %s after cancellation", elapsed)
	}
}

func TestSubmitUnexpectedTaskResultFailsContractMismatch(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	f.taskResult.Store(true)
	svc, _, _ := fixtureApp(t, f)

	id := submitStatus(t, svc, "task-mismatch")
	out := awaitTerminal(t, svc, id)
	if out.Status != contract.StatusFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.Error == nil || out.Error.Code != contract.CodeOperationContractMismatch {
		t.Fatalf("error = %+v, want operation_contract_mismatch", out.Error)
	}
	// The dedicated unexpected-task message, not the catalog-drift message:
	// the two causes share a code but must stay distinguishable.
	const want = "The upstream returned a task result for a pinned synchronous operation."
	if out.Error.Message != want {
		t.Fatalf("message = %q, want %q", out.Error.Message, want)
	}
	if out.Error.Retryable {
		t.Fatalf("error = %+v, must not be retryable", out.Error)
	}
}

// TestSubmitStaleDescriptorDigestFailsContractMismatch pins the replay
// contract: a durable submission whose accepted descriptor digest does not
// match the connection's effective descriptor is not executed. This is the
// crash-reconcile scenario: the profile changed to a different descriptor
// under the same tool name while the submission was pending.
func TestSubmitStaleDescriptorDigestFailsContractMismatch(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, st, _ := fixtureApp(t, f)

	// Create the durable submission directly with a foreign accepted
	// digest: the store takes the digest verbatim at creation, which is
	// exactly what a reconciled profile would have recorded before the
	// change.
	sub, _, err := st.CreateSubmission(context.Background(), store.NewSubmission{
		ID:               "sub-stale-digest",
		ClientRequestID:  "stale-digest",
		Tool:             "status",
		Strategy:         string(catalog.StrategyLocalReplayable),
		DescriptorDigest: "sha256:stale-different-contract",
		Arguments:        json.RawMessage(`{"detail":"unit"}`),
		ProtocolVersion:  "2026-07-28",
		AdapterVersion:   "test-adapter",
	})
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}

	out := awaitTerminal(t, svc, sub.ID)
	if out.Status != contract.StatusFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.Error == nil || out.Error.Code != contract.CodeOperationContractMismatch {
		t.Fatalf("error = %+v, want operation_contract_mismatch", out.Error)
	}
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("upstream received %d calls, want 0 (stale digest must fail before execution)", got)
	}
}

func TestSubmitResultTooLargeFails(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	lim := limits.Default()
	lim.ResultBytes = 16 // the fixture result is larger than this
	svc, _, _ := fixtureAppLimits(t, f, lim)

	id := submitStatus(t, svc, "large-1")
	out := awaitTerminal(t, svc, id)
	if out.Status != contract.StatusFailed {
		t.Fatalf("status = %s, want failed", out.Status)
	}
	if out.Error == nil || out.Error.Code != contract.CodeResultTooLarge {
		t.Fatalf("error = %+v, want result_too_large", out.Error)
	}
	if out.Result != nil {
		t.Fatalf("oversized result must never be returned: %+v", out.Result)
	}
}
