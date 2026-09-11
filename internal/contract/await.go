package contract

import "time"

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

// Content is one normalized text content block of a terminal result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Result is the captured terminal result of a completed operation. A
// completed operation may contain IsError true; the captured result is
// returned exactly as stored and is never truncated.
type Result struct {
	IsError           bool           `json:"is_error"`
	Content           []Content      `json:"content"`
	StructuredContent map[string]any `json:"structured_content,omitempty"`
}

// AwaitOutput is the client-facing output for the await tool. Every
// successful response contains the current snapshot and the events after the
// supplied cursor. Request-level failures carry only the error.
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
