package application

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kritama/tama-link/internal/contract"
)

func TestSubmitReplayableCompletes(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)

	id := submitStatus(t, svc, "req-1")
	if id == "" {
		t.Fatal("empty submission id")
	}

	out := awaitTerminal(t, svc, id)
	if out.Status != contract.StatusCompleted {
		t.Fatalf("status = %s, want completed", out.Status)
	}
	if out.Result == nil {
		t.Fatal("missing terminal result")
	}
	var structured struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(out.Result.StructuredContent, &structured); err != nil {
		t.Fatalf("result structured content: %v", err)
	}
	if !structured.OK {
		t.Fatalf("result = %s", string(out.Result.StructuredContent))
	}
	if f.calls.Load() != 1 {
		t.Fatalf("tools/call executed %d times, want exactly 1", f.calls.Load())
	}
}

func TestSubmitRejectedCases(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)

	cases := []struct {
		name string
		in   contract.SubmitInput
		want contract.Code
	}{
		{
			name: "unknown tool",
			in:   contract.SubmitInput{Tool: "nope", ClientRequestID: "r-1", Arguments: json.RawMessage(`{}`)},
			want: contract.CodeOperationNotAllowed,
		},
		{
			name: "missing client request id",
			in:   contract.SubmitInput{Tool: "status", Arguments: json.RawMessage(`{}`)},
			want: contract.CodeInvalidRequest,
		},
		{
			name: "arguments fail client schema",
			in: contract.SubmitInput{
				Tool: "message", ClientRequestID: "r-2",
				Arguments: json.RawMessage(`{"message":""}`),
			},
			want: contract.CodeInvalidRequest,
		},
		{
			name: "arguments not an object",
			in:   contract.SubmitInput{Tool: "status", ClientRequestID: "r-3", Arguments: json.RawMessage(`[1]`)},
			want: contract.CodeInvalidRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, appErr := svc.Submit(context.Background(), tc.in)
			if appErr == nil || appErr.Code != tc.want {
				t.Fatalf("error = %+v, want %s", appErr, tc.want)
			}
		})
	}
}

func TestSubmitTaskToolNotEnabled(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)

	_, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "message",
		ClientRequestID: "r-task",
		Arguments:       json.RawMessage(`{"message":"hi"}`),
	})
	if appErr == nil || appErr.Code != contract.CodeNotImplemented {
		t.Fatalf("error = %+v, want not_implemented", appErr)
	}
	if f.calls.Load() != 0 {
		t.Fatalf("fixture upstream was touched: %d calls", f.calls.Load())
	}
}

func TestSubmitIdempotency(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)

	first := submitStatus(t, svc, "dup-1")

	out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "dup-1",
		ClientContext:   &contract.ClientContext{ThreadID: "thread-1"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	})
	if appErr != nil {
		t.Fatalf("retry: %s", appErr.Message)
	}
	if out.SubmissionID != first {
		t.Fatalf("retry returned %s, want %s", out.SubmissionID, first)
	}

	conflict, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "dup-1",
		Arguments:       json.RawMessage(`{"detail":"different"}`),
	})
	if appErr == nil || appErr.Code != contract.CodeIdempotencyConflict {
		t.Fatalf("conflict = %+v, want idempotency_conflict (submission %+v)", appErr, conflict)
	}
}

func TestSubmitAppliesBindings(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)
	// The fixture profile pins bindings on the status tool.
	id := submitStatus(t, svc, "bind-1")
	awaitTerminal(t, svc, id)

	f.mu.Lock()
	defer f.mu.Unlock()
	var call struct {
		Params struct {
			Arguments struct {
				Detail     string `json:"detail"`
				ClientMeta struct {
					RequestID string `json:"client_request_id"`
				} `json:"client_meta"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(f.lastCallDoc), &call); err != nil {
		t.Fatalf("last call: %v", err)
	}
	if call.Params.Arguments.Detail != "unit" {
		t.Fatalf("detail = %q", call.Params.Arguments.Detail)
	}
	if call.Params.Arguments.ClientMeta.RequestID != "bind-1" {
		t.Fatalf("binding not applied: %+v", call.Params.Arguments.ClientMeta)
	}
}
