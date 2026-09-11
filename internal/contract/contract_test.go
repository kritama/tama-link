package contract

import (
	"encoding/json"
	"testing"
	"time"
)

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return string(encoded)
}

func TestErrorEnvelope(t *testing.T) {
	t.Parallel()

	err := NewError(CodeUpstreamExecutionFailed, "Safe user-facing summary")
	got := mustMarshal(t, err)
	want := `{"code":"upstream_execution_failed","message":"Safe user-facing summary","retryable":false}`
	if got != want {
		t.Fatalf("error JSON = %s, want %s", got, want)
	}
}

func TestErrorDefaultRetryability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code      Code
		retryable bool
	}{
		{CodeInvalidRequest, false},
		{CodeOperationNotAllowed, false},
		{CodeOperationContractMismatch, false},
		{CodeIdempotencyConflict, false},
		{CodeSubmissionNotFound, false},
		{CodeSubmissionExpired, false},
		{CodeAuthenticationRequired, false},
		{CodeAuthorizationFailed, false},
		{CodeProtocolMismatch, false},
		{CodeUpstreamUnavailable, true},
		{CodeUpstreamExecutionFailed, false},
		{CodeOutcomeUnknown, false},
		{CodeResultTooLarge, false},
		{CodeStateUnavailable, false},
		{CodeInternal, false},
		{CodeNotImplemented, false},
	}
	for _, test := range tests {
		err := NewError(test.code, "message")
		if err.Retryable != test.retryable {
			t.Fatalf("code %s retryable = %v, want %v", test.code, err.Retryable, test.retryable)
		}
		if err.Code != test.code || err.Message != "message" {
			t.Fatalf("code %s = %+v", test.code, err)
		}
	}
}

func TestSubmitInputOmitsEmptyFields(t *testing.T) {
	t.Parallel()

	got := mustMarshal(t, SubmitInput{Tool: "message"})
	want := `{"tool":"message"}`
	if got != want {
		t.Fatalf("submit input JSON = %s, want %s", got, want)
	}
}

func TestSubmitOutputSuccessMatchesSpec(t *testing.T) {
	t.Parallel()

	submittedAt := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	out := SubmitOutput{
		SubmissionID:    "sub_opaque",
		Status:          StatusAccepted,
		ClientRequestID: "opaque-idempotency-key",
		SubmittedAt:     &submittedAt,
		NextPollMS:      1000,
	}
	got := mustMarshal(t, out)
	want := `{"submission_id":"sub_opaque","status":"accepted","client_request_id":"opaque-idempotency-key","submitted_at":"2026-09-11T12:00:00Z","next_poll_ms":1000}`
	if got != want {
		t.Fatalf("submit output JSON = %s, want %s", got, want)
	}
}

func TestSubmitOutputFailureCarriesOnlyError(t *testing.T) {
	t.Parallel()

	err := NewError(CodeInvalidRequest, "tool is not allowed by the selected profile")
	got := mustMarshal(t, SubmitOutput{Error: &err})
	want := `{"error":{"code":"invalid_request","message":"tool is not allowed by the selected profile","retryable":false}}`
	if got != want {
		t.Fatalf("submit output JSON = %s, want %s", got, want)
	}
}

func TestAwaitOutputPendingMatchesSpec(t *testing.T) {
	t.Parallel()

	out := AwaitOutput{
		SubmissionID: "sub_opaque",
		Status:       StatusRunning,
		Terminal:     false,
		Cursor:       "event_cursor",
		Progress: &Progress{
			Step:    Step{Current: 2, Total: 4, Label: "Indexing memories"},
			Message: "Processing 18 posts",
		},
		Events:     []Event{},
		NextPollMS: 1000,
	}
	got := mustMarshal(t, out)
	want := `{"submission_id":"sub_opaque","status":"running","terminal":false,"cursor":"event_cursor","progress":{"current":2,"total":4,"label":"Indexing memories","message":"Processing 18 posts"},"events":[],"next_poll_ms":1000}`
	if got != want {
		t.Fatalf("await output JSON = %s, want %s", got, want)
	}
}

func TestAwaitOutputCompletedMatchesSpec(t *testing.T) {
	t.Parallel()

	completedAt := time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)
	out := AwaitOutput{
		SubmissionID: "sub_opaque",
		Tool:         "message",
		Status:       StatusCompleted,
		Terminal:     true,
		Cursor:       "event_cursor",
		Events:       []Event{},
		Result: &Result{
			IsError:           false,
			Content:           []Content{{Type: "text", Text: "Saved."}},
			StructuredContent: map[string]any{"saved": true},
		},
		CompletedAt: &completedAt,
	}
	got := mustMarshal(t, out)
	want := `{"submission_id":"sub_opaque","tool":"message","status":"completed","terminal":true,"cursor":"event_cursor","events":[],"result":{"is_error":false,"content":[{"type":"text","text":"Saved."}],"structured_content":{"saved":true}},"completed_at":"2026-09-11T12:05:00Z"}`
	if got != want {
		t.Fatalf("await output JSON = %s, want %s", got, want)
	}
}

func TestAwaitOutputFailedMatchesSpec(t *testing.T) {
	t.Parallel()

	completedAt := time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)
	err := NewError(CodeUpstreamExecutionFailed, "Safe user-facing summary")
	out := AwaitOutput{
		SubmissionID: "sub_opaque",
		Status:       StatusFailed,
		Terminal:     true,
		Cursor:       "event_cursor",
		Events:       []Event{},
		Error:        &err,
		CompletedAt:  &completedAt,
	}
	got := mustMarshal(t, out)
	want := `{"submission_id":"sub_opaque","status":"failed","terminal":true,"cursor":"event_cursor","events":[],"error":{"code":"upstream_execution_failed","message":"Safe user-facing summary","retryable":false},"completed_at":"2026-09-11T12:05:00Z"}`
	if got != want {
		t.Fatalf("await output JSON = %s, want %s", got, want)
	}
}

func TestEventMatchesSpec(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 9, 11, 12, 0, 7, 0, time.UTC)
	event := Event{
		SubmissionID: "sub_opaque",
		Sequence:     7,
		Timestamp:    ts,
		State:        StatusRunning,
		Step:         &Step{Current: 2, Total: 4, Label: "Indexing memories"},
		Message:      "Processing 18 posts",
	}
	got := mustMarshal(t, event)
	want := `{"submission_id":"sub_opaque","sequence":7,"timestamp":"2026-09-11T12:00:07Z","state":"running","step":{"current":2,"total":4,"label":"Indexing memories"},"message":"Processing 18 posts"}`
	if got != want {
		t.Fatalf("event JSON = %s, want %s", got, want)
	}
}

func TestProgressOmitsUnknownValues(t *testing.T) {
	t.Parallel()

	progress := &Progress{Message: "Queued"}
	got := mustMarshal(t, progress)
	want := `{"message":"Queued"}`
	if got != want {
		t.Fatalf("progress JSON = %s, want %s", got, want)
	}
}
