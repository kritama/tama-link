package upstream

import (
	"context"
	"encoding/json"
	"fmt"
)

// CallToolParams selects one tool call.
type CallToolParams struct {
	// Name is the tool name; it also becomes the Mcp-Name header.
	Name string
	// Arguments is the validated canonical upstream argument object. It is
	// sent verbatim; nil or empty means the argument object is omitted.
	Arguments json.RawMessage
	// Capabilities, when non-nil, replaces the client's declared
	// capabilities in this one request's _meta triple. This is how the
	// adapter declares the Tasks extension only for operations that may
	// execute as tasks.
	Capabilities json.RawMessage
}

// CallToolResponse is a tools/call result in either result shape.
type CallToolResponse struct {
	// Raw is the complete result value, lossless.
	Raw json.RawMessage
	// ResultType is "complete" for a synchronous CallToolResult and "task"
	// for a server-directed task snapshot.
	ResultType string
	// TaskID is set when ResultType is "task".
	TaskID string
	// Status, StatusMessage, CreatedAt, LastUpdatedAt, TTLMs, and
	// PollIntervalMs describe the task snapshot when ResultType is "task".
	Status         string
	StatusMessage  string
	CreatedAt      string
	LastUpdatedAt  string
	TTLMs          int64
	PollIntervalMs int64
}

// IsTask reports whether the response carries a server-directed task.
func (r *CallToolResponse) IsTask() bool { return r.ResultType == resultTypeTask }

// CallTool issues one tools/call and decodes either result shape. The task
// capability must already be declared in the client capabilities for
// task-backed operations.
func (c *Client) CallTool(ctx context.Context, p *CallToolParams) (*CallToolResponse, error) {
	if p == nil || p.Name == "" {
		return nil, fmt.Errorf("tool name is required")
	}
	wire, err := json.Marshal(wireCallTool{Name: p.Name, Arguments: p.Arguments})
	if err != nil {
		return nil, fmt.Errorf("encode tools/call params: %w", err)
	}
	var meta json.RawMessage
	if p.Capabilities != nil {
		if !IsJSONObject(p.Capabilities) {
			return nil, fmt.Errorf("per-request capabilities must be a JSON object")
		}
		meta, err = buildMetaWith(c.info, p.Capabilities)
		if err != nil {
			return nil, err
		}
	}
	raw, err := c.callWithMeta(ctx, MethodCallTool, p.Name, wire, meta)
	if err != nil {
		return nil, err
	}
	return decodeCallTool(raw)
}

// wireCallTool is the tools/call params object. Arguments stays raw.
type wireCallTool struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// resultType values on tools/call and tasks results.
const (
	resultTypeComplete = "complete"
	resultTypeTask     = "task"
)

// decodeCallTool projects a tools/call result, failing closed on an unknown
// resultType.
func decodeCallTool(raw json.RawMessage) (*CallToolResponse, error) {
	var view struct {
		ResultType     string `json:"resultType"`
		TaskID         string `json:"taskId"`
		Status         string `json:"status"`
		StatusMessage  string `json:"statusMessage"`
		CreatedAt      string `json:"createdAt"`
		LastUpdatedAt  string `json:"lastUpdatedAt"`
		TTLMs          int64  `json:"ttlMs"`
		PollIntervalMs int64  `json:"pollIntervalMs"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("decode tools/call result: %w", err)
	}
	switch view.ResultType {
	case resultTypeComplete, resultTypeTask:
	default:
		return nil, newError(KindProtocol, 0, fmt.Errorf("unknown tools/call resultType %q", view.ResultType))
	}
	if view.ResultType == resultTypeTask {
		if view.TaskID == "" {
			return nil, newError(KindProtocol, 0, fmt.Errorf("task result has no task id"))
		}
		// The pinned TamaMCP profile creates tasks in the working state; a
		// terminal or unknown initial state is a protocol failure, not a
		// shortcut to a captured result.
		if view.Status != TaskWorking {
			return nil, newError(KindProtocol, 0, fmt.Errorf("initial task state %q is not working", view.Status))
		}
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil {
			return nil, newError(KindProtocol, 0, fmt.Errorf("initial task result is not a JSON object"))
		}
		if err := validateTaskEnvelope(members); err != nil {
			return nil, err
		}
	}
	return &CallToolResponse{
		Raw:            raw,
		ResultType:     view.ResultType,
		TaskID:         view.TaskID,
		Status:         view.Status,
		StatusMessage:  view.StatusMessage,
		CreatedAt:      view.CreatedAt,
		LastUpdatedAt:  view.LastUpdatedAt,
		TTLMs:          view.TTLMs,
		PollIntervalMs: view.PollIntervalMs,
	}, nil
}
