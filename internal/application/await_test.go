package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
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
	if out.Error.Message == "" || out.Error.Retryable {
		t.Fatalf("error = %+v, want non-retryable with a message", out.Error)
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
