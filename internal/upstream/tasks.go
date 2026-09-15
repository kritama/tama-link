package upstream

import (
	"context"
	"encoding/json"
	"fmt"
)

// MCP task methods. All three set Mcp-Name to the task ID.
const (
	MethodTaskGet    = "tasks/get"
	MethodTaskUpdate = "tasks/update"
	MethodTaskCancel = "tasks/cancel"
)

// Task states on the TamaMCP 2026-07-28 Tasks extension. completed, failed,
// and cancelled are terminal.
const (
	TaskWorking       = "working"
	TaskInputRequired = "input_required"
	TaskCompleted     = "completed"
	TaskFailed        = "failed"
	TaskCancelled     = "cancelled"
)

// ValidTaskStatus reports whether s is a known task state.
func ValidTaskStatus(s string) bool {
	switch s {
	case TaskWorking, TaskInputRequired, TaskCompleted, TaskFailed, TaskCancelled:
		return true
	default:
		return false
	}
}

// TaskState is the detailed task state returned by tasks/get and carried in
// notifications/tasks. It is the recovery source of truth: it includes the
// complete terminal CallToolResult, the structured failure, or the
// outstanding input-request map, depending on state.
type TaskState struct {
	// Raw is the complete state document, lossless.
	Raw json.RawMessage
	// ResultType is "complete" on tasks/get envelopes. It is absent on
	// notification payloads and is zero then.
	ResultType string
	// TaskID identifies the task.
	TaskID string
	// Status is one of the Task* states.
	Status string
	// StatusMessage is the bounded human-readable status.
	StatusMessage string
	// CreatedAt and LastUpdatedAt are RFC3339 timestamps.
	CreatedAt     string
	LastUpdatedAt string
	// TTLMs bounds task access from creation.
	TTLMs int64
	// PollIntervalMs is the suggested polling interval.
	PollIntervalMs int64
	// Result is the complete CallToolResult when status is completed.
	Result json.RawMessage
	// Error is the structured failure when status is failed.
	Error json.RawMessage
	// InputRequests is the outstanding input-request map when status is
	// input_required.
	InputRequests json.RawMessage
}

// IsTerminal reports whether the state is terminal.
func (t *TaskState) IsTerminal() bool {
	return t.Status == TaskCompleted || t.Status == TaskFailed || t.Status == TaskCancelled
}

// TaskGet retrieves the detailed state for one owner-bound task. Task IDs
// are durable and independent of any HTTP connection; lookup is
// authenticated independently on every call.
func (c *Client) TaskGet(ctx context.Context, taskID string) (*TaskState, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	raw, err := c.call(ctx, MethodTaskGet, taskID, json.RawMessage(`{"taskId":`+jsonString(taskID)+`}`))
	if err != nil {
		return nil, err
	}
	return decodeTaskState(raw, taskID)
}

// TaskUpdate sends input responses for a task in input_required. The
// acknowledgement is eventually consistent: it does not prove the task left
// input_required.
func (c *Client) TaskUpdate(ctx context.Context, taskID string, inputResponses json.RawMessage) (json.RawMessage, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	if !isJSONObject(inputResponses) {
		return nil, fmt.Errorf("input responses must be a JSON object")
	}
	params := `{"taskId":` + jsonString(taskID) + `,"inputResponses":` + string(inputResponses) + `}`
	return c.call(ctx, MethodTaskUpdate, taskID, json.RawMessage(params))
}

// TaskCancel records cooperative cancellation intent. It does not assert the
// task reached cancelled; the runner may still commit completed or failed.
// Tama Link downstream wait cancellation must never call this method.
func (c *Client) TaskCancel(ctx context.Context, taskID string) (json.RawMessage, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	raw, err := c.call(ctx, MethodTaskCancel, taskID, json.RawMessage(`{"taskId":`+jsonString(taskID)+`}`))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// decodeTaskState projects and validates one detailed task state. Unknown
// states and cross-state payloads fail closed.
func decodeTaskState(raw json.RawMessage, expectedTaskID string) (*TaskState, error) {
	var view struct {
		ResultType     string          `json:"resultType"`
		TaskID         string          `json:"taskId"`
		Status         string          `json:"status"`
		StatusMessage  string          `json:"statusMessage"`
		CreatedAt      string          `json:"createdAt"`
		LastUpdatedAt  string          `json:"lastUpdatedAt"`
		TTLMs          int64           `json:"ttlMs"`
		PollIntervalMs int64           `json:"pollIntervalMs"`
		Result         json.RawMessage `json:"result"`
		Error          json.RawMessage `json:"error"`
		InputRequests  json.RawMessage `json:"inputRequests"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("decode task state: %w", err)
	}
	if view.TaskID == "" || (expectedTaskID != "" && view.TaskID != expectedTaskID) {
		return nil, newError(KindProtocol, 0, fmt.Errorf("task state id mismatch"))
	}
	if !ValidTaskStatus(view.Status) {
		return nil, newError(KindProtocol, 0, fmt.Errorf("unknown task status %q", view.Status))
	}
	if err := checkStatePayloads(view.Status, view.Result, view.Error, view.InputRequests); err != nil {
		return nil, err
	}
	return &TaskState{
		Raw:            raw,
		ResultType:     view.ResultType,
		TaskID:         view.TaskID,
		Status:         view.Status,
		StatusMessage:  view.StatusMessage,
		CreatedAt:      view.CreatedAt,
		LastUpdatedAt:  view.LastUpdatedAt,
		TTLMs:          view.TTLMs,
		PollIntervalMs: view.PollIntervalMs,
		Result:         view.Result,
		Error:          view.Error,
		InputRequests:  view.InputRequests,
	}, nil
}

// checkStatePayloads enforces the state-specific payload contract: a
// completed state carries the result, a failed state carries the error, an
// input_required state carries the outstanding requests, and none of the
// other states may carry a foreign payload.
func checkStatePayloads(status string, result, taskErr, inputRequests json.RawMessage) error {
	present := func(raw json.RawMessage) bool { return len(raw) > 0 }
	switch status {
	case TaskCompleted:
		if !present(result) {
			return newError(KindProtocol, 0, fmt.Errorf("completed task state has no result"))
		}
	case TaskFailed:
		if !present(taskErr) {
			return newError(KindProtocol, 0, fmt.Errorf("failed task state has no error"))
		}
	case TaskInputRequired:
		if !present(inputRequests) {
			return newError(KindProtocol, 0, fmt.Errorf("input_required task state has no input requests"))
		}
	default:
		if present(result) || present(taskErr) || present(inputRequests) {
			return newError(KindProtocol, 0, fmt.Errorf("non-terminal task state carries a terminal payload"))
		}
	}
	if (present(result) && present(taskErr)) || (present(result) && present(inputRequests)) || (present(taskErr) && present(inputRequests)) {
		return newError(KindProtocol, 0, fmt.Errorf("task state carries conflicting payloads"))
	}
	return nil
}

// jsonString renders a string as a JSON literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
