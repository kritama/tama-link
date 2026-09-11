package contract

import (
	"bytes"
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

func TestErrorOutputCarriesOnlyError(t *testing.T) {
	t.Parallel()

	err := NewError(CodeInvalidRequest, "tool is not allowed by the selected profile")
	got := mustMarshal(t, ErrorOutput{Error: &err})
	want := `{"error":{"code":"invalid_request","message":"tool is not allowed by the selected profile","retryable":false}}`
	if got != want {
		t.Fatalf("error output JSON = %s, want %s", got, want)
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
			Content:           []ContentBlock{json.RawMessage(`{"type":"text","text":"Saved."}`)},
			StructuredContent: json.RawMessage(`{"saved":true}`),
		},
		CompletedAt: &completedAt,
	}
	got := mustMarshal(t, out)
	want := `{"submission_id":"sub_opaque","tool":"message","status":"completed","terminal":true,"cursor":"event_cursor","events":[],"result":{"is_error":false,"content":[{"type":"text","text":"Saved."}],"structured_content":{"saved":true}},"completed_at":"2026-09-11T12:05:00Z"}`
	if got != want {
		t.Fatalf("await output JSON = %s, want %s", got, want)
	}
}

// TestResultRoundTrip proves the normalized result model is lossless for
// every allowed MCP tool result content block and structured content shape.
func TestResultRoundTrip(t *testing.T) {
	t.Parallel()

	fixtures := map[string]string{
		"text_with_annotations_and_meta": `{"is_error":false,"content":[{"type":"text","text":"Saved.","annotations":{"audience":["assistant"],"priority":0.5,"lastModified":"2026-09-11T12:00:00Z"},"_meta":{"trace":"t-1"}}],"structured_content":{"ok":true},"_meta":{"safe":"yes"}}`,
		"image":                          `{"is_error":false,"content":[{"type":"image","data":"aGVsbG8=","mimeType":"image/png"}]}`,
		"audio":                          `{"is_error":false,"content":[{"type":"audio","data":"aGVsbG8=","mimeType":"audio/wav"}]}`,
		"resource_link":                  `{"is_error":false,"content":[{"type":"resource_link","uri":"file:///tmp/x","name":"x","title":"X","description":"d","mimeType":"text/plain","size":4,"icons":[{"src":"https://example.com/x.png","mimeType":"image/png"}],"annotations":{"priority":1}}]}`,
		"embedded_resource":              `{"is_error":false,"content":[{"type":"resource","resource":{"uri":"file:///tmp/x","mimeType":"text/plain","text":"body","_meta":{"etag":"v1"}},"annotations":{"audience":["user"]}}]}`,
		"embedded_resource_blob":         `{"is_error":false,"content":[{"type":"resource","resource":{"uri":"file:///tmp/x","blob":"aGVsbG8="}}]}`,
		"unknown_extension_block":        `{"is_error":false,"content":[{"type":"future_content","precise":9007199254740993,"extension":{"nested":true}}]}`,
		"array_structured_is_error":      `{"is_error":true,"content":[],"structured_content":[1,2,3]}`,
		"primitive_structured":           `{"is_error":false,"content":[],"structured_content":"done"}`,
		"null_structured":                `{"is_error":false,"content":[],"structured_content":null}`,
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var in Result
			if err := json.Unmarshal([]byte(fixture), &in); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			out, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("remarshal: %v", err)
			}

			var wantValue, gotValue any
			if err := json.Unmarshal([]byte(fixture), &wantValue); err != nil {
				t.Fatalf("unmarshal want: %v", err)
			}
			if err := json.Unmarshal(out, &gotValue); err != nil {
				t.Fatalf("unmarshal got: %v", err)
			}
			wantJSON, _ := json.Marshal(wantValue)
			gotJSON, _ := json.Marshal(gotValue)
			if !bytes.Equal(wantJSON, gotJSON) {
				t.Fatalf("round trip changed the result:\nwant %s\ngot  %s", wantJSON, gotJSON)
			}
		})
	}
}

func TestResultValidation(t *testing.T) {
	t.Parallel()

	for name, result := range map[string]Result{
		"primitive content": {Content: []ContentBlock{json.RawMessage(`"text"`)}},
		"missing type":      {Content: []ContentBlock{json.RawMessage(`{"text":"hello"}`)}},
		"invalid structured": {
			Content:           []ContentBlock{},
			StructuredContent: json.RawMessage(`{`),
		},
		"primitive meta": {Content: []ContentBlock{}, Meta: json.RawMessage(`1`)},
	} {
		if err := result.Validate(); err == nil {
			t.Fatalf("%s result accepted", name)
		}
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
