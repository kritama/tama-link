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

// Annotation is a reviewed content annotation carried with a content block.
type Annotation struct {
	Audience []string `json:"audience,omitempty"`
	Priority *float64 `json:"priority,omitempty"`
}

// EmbeddedResource is the resource payload of an embedded_resource content
// block. Text and blob are mutually exclusive upstream.
type EmbeddedResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// ContentBlock is the lossless normalized form of one MCP tool result
// content block. Fields apply per block type: text to Text; data and mimeType
// to image and audio; uri, name, title, description, and mimeType to
// resource_link; resource to embedded_resource. Unknown block types keep
// their type and round-trip their preserved fields.
type ContentBlock struct {
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	Data        string            `json:"data,omitempty"`
	MimeType    string            `json:"mimeType,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Name        string            `json:"name,omitempty"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Resource    *EmbeddedResource `json:"resource,omitempty"`
	Annotations []Annotation      `json:"annotations,omitempty"`
	Meta        map[string]any    `json:"_meta,omitempty"`
}

// Result is the captured terminal result of a completed operation. It is a
// lossless normalized MCP CallToolResult: structured content may be any valid
// JSON value, and reviewed safe _meta is preserved at the result and
// content-block boundaries. A completed operation may contain IsError true;
// the captured result is returned exactly as stored and is never truncated.
type Result struct {
	IsError           bool            `json:"is_error"`
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structured_content,omitempty"`
	Meta              map[string]any  `json:"_meta,omitempty"`
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
