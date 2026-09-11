package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ClientContext carries client-owned correlation values that an operation
// binding may require but ordinary MCP does not provide automatically.
type ClientContext struct {
	ThreadID string `json:"thread_id,omitempty" jsonschema:"host conversation identifier supplied by the client"`
}

// SubmitInput is the client-facing input for the submit tool.
type SubmitInput struct {
	Tool            string          `json:"tool" jsonschema:"upstream Tama tool name allowed by the selected profile"`
	Arguments       json.RawMessage `json:"arguments,omitempty" jsonschema:"arguments for the upstream Tama tool"`
	ClientRequestID string          `json:"client_request_id,omitempty" jsonschema:"opaque idempotency key scoped to the selected profile"`
	ClientContext   *ClientContext  `json:"client_context,omitempty" jsonschema:"client-owned correlation values"`
}

// DecodeSubmitInput decodes the raw downstream tool arguments without routing
// arbitrary JSON numbers through float64. The low-level MCP handler calls this
// directly because the SDK's typed handler normalizes inputs through
// map[string]any before invoking application code.
func DecodeSubmitInput(raw json.RawMessage) (SubmitInput, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}

	var input SubmitInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		return SubmitInput{}, fmt.Errorf("decode submit input: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return SubmitInput{}, fmt.Errorf("submit input contains trailing data")
	}
	if input.Tool == "" {
		return SubmitInput{}, fmt.Errorf("tool is required")
	}
	if len(input.Arguments) == 0 {
		input.Arguments = json.RawMessage(`{}`)
	}
	if err := requireJSONObject(input.Arguments); err != nil {
		return SubmitInput{}, fmt.Errorf("arguments: %w", err)
	}
	return input, nil
}

func requireJSONObject(raw json.RawMessage) error {
	var object map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&object); err != nil {
		return fmt.Errorf("must be a JSON object: %w", err)
	}
	if object == nil {
		return fmt.Errorf("must be a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("must contain one JSON object")
	}
	return nil
}

// SubmitOutput is the client-facing success output for the submit tool.
// Request-level failures return ErrorOutput instead.
type SubmitOutput struct {
	SubmissionID    string     `json:"submission_id,omitempty"`
	Status          Status     `json:"status,omitempty"`
	ClientRequestID string     `json:"client_request_id,omitempty"`
	SubmittedAt     *time.Time `json:"submitted_at,omitempty"`
	NextPollMS      int        `json:"next_poll_ms,omitempty"`
}
