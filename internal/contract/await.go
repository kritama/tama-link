package contract

import (
	"encoding/json"
	"time"
)

// AwaitInput is the client-facing input for the await tool.
type AwaitInput struct {
	SubmissionID string `json:"submission_id" jsonschema:"opaque Tama Link submission identifier"`
	Cursor       string `json:"cursor,omitempty" jsonschema:"opaque progress cursor returned by an earlier await call"`
	TimeoutMS    int    `json:"timeout_ms,omitempty" jsonschema:"bounded long-poll duration in milliseconds"`
}

// Step is a bounded step counter within an operation. Unknown values are
// omitted, not represented as zero.
type Step struct {
	Current int    `json:"current,omitempty"`
	Total   int    `json:"total,omitempty"`
	Label   string `json:"label,omitempty"`
}

// Progress is the await snapshot progress. Unknown values are omitted.
type Progress struct {
	Step
	Message string `json:"message,omitempty"`
}

// Event is one normalized progress event. Sequences are strictly increasing
// within one submission; consumers deduplicate by (submission, sequence).
type Event struct {
	SubmissionID string    `json:"submission_id"`
	Sequence     int64     `json:"sequence"`
	Timestamp    time.Time `json:"timestamp"`
	State        Status    `json:"state"`
	Step         *Step     `json:"step,omitempty"`
	Message      string    `json:"message,omitempty"`
}

// ContentBlock holds one validated MCP content block as raw JSON. Keeping the
// complete block avoids losing fields added by a newer upstream MCP revision,
// including extension metadata. Adapters validate the block before it crosses
// this boundary; the store preserves the accepted bytes without projection.
type ContentBlock = json.RawMessage

// Result is the captured terminal result of a completed operation. It is a
// lossless normalized MCP CallToolResult: structured content may be any valid
// JSON value, and reviewed safe _meta is preserved at the result and
// content-block boundaries. A completed operation may contain IsError true;
// the captured result is returned exactly as stored and is never truncated.
type Result struct {
	IsError           bool            `json:"is_error"`
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structured_content,omitempty"`
	Meta              json.RawMessage `json:"_meta,omitempty"`
}

// ErrorOutput is the request-level failure envelope for both tools. It
// carries only the error: no submission was accepted or is referenced.
// Terminal submission failures instead appear in AwaitOutput.Error.
type ErrorOutput struct {
	Error *Error `json:"error"`
}

// AwaitOutput is the client-facing output for the await tool. Every
// successful response contains the current snapshot and the events after the
// supplied cursor. A submission that reached a non-completed terminal state
// carries its stable structured error. Request-level failures return
// ErrorOutput instead.
type AwaitOutput struct {
	SubmissionID string     `json:"submission_id"`
	Tool         string     `json:"tool,omitempty"`
	Status       Status     `json:"status,omitempty"`
	Terminal     bool       `json:"terminal"`
	Cursor       string     `json:"cursor,omitempty"`
	Progress     *Progress  `json:"progress,omitempty"`
	Events       []Event    `json:"events"`
	Result       *Result    `json:"result,omitempty"`
	Error        *Error     `json:"error,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	NextPollMS   int        `json:"next_poll_ms,omitempty"`
}
