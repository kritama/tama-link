package contract

import "time"

// ClientContext carries client-owned correlation values that an operation
// binding may require but ordinary MCP does not provide automatically.
type ClientContext struct {
	ThreadID string `json:"thread_id,omitempty" jsonschema:"host conversation identifier supplied by the client"`
}

// SubmitInput is the client-facing input for the submit tool.
type SubmitInput struct {
	Tool            string         `json:"tool" jsonschema:"upstream Tama tool name allowed by the selected profile"`
	Arguments       map[string]any `json:"arguments,omitempty" jsonschema:"arguments for the upstream Tama tool"`
	ClientRequestID string         `json:"client_request_id,omitempty" jsonschema:"opaque idempotency key scoped to the selected profile"`
	ClientContext   *ClientContext `json:"client_context,omitempty" jsonschema:"client-owned correlation values"`
}

// SubmitOutput is the client-facing output for the submit tool. On success
// the error field is nil and the submission fields are set; on failure only
// the error field is set.
type SubmitOutput struct {
	SubmissionID    string     `json:"submission_id,omitempty"`
	Status          Status     `json:"status,omitempty"`
	ClientRequestID string     `json:"client_request_id,omitempty"`
	SubmittedAt     *time.Time `json:"submitted_at,omitempty"`
	NextPollMS      int        `json:"next_poll_ms,omitempty"`
	Error           *Error     `json:"error,omitempty"`
}
