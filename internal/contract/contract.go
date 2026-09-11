// Package contract defines Tama Link's stable client-facing MCP tool
// contract: the input and output documents for the submit and await tools,
// the normalized progress model, submission status values, and the stable
// error taxonomy.
//
// These types are the compatibility API. JSON field names and status or
// error values are part of the public surface and change only through a
// specification change.
package contract

// Tool names exposed downstream. The surface is exactly these two tools.
const (
	ToolSubmit = "submit"
	ToolAwait  = "await"
)

// Status is a normalized submission status exposed in tool output.
type Status string

// Normalized submission statuses.
const (
	StatusAccepted       Status = "accepted"
	StatusQueued         Status = "queued"
	StatusRunning        Status = "running"
	StatusCompleted      Status = "completed"
	StatusFailed         Status = "failed"
	StatusCancelled      Status = "cancelled"
	StatusExpired        Status = "expired"
	StatusOutcomeUnknown Status = "outcome_unknown"
)
