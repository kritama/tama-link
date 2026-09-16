package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
	return decodeTaskState(raw, taskID, true)
}

// TaskUpdate sends input responses for a task in input_required. The
// acknowledgement is eventually consistent: it does not prove the task left
// input_required.
func (c *Client) TaskUpdate(ctx context.Context, taskID string, inputResponses json.RawMessage) (json.RawMessage, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	if !IsJSONObject(inputResponses) {
		return nil, fmt.Errorf("input responses must be a JSON object")
	}
	params := `{"taskId":` + jsonString(taskID) + `,"inputResponses":` + string(inputResponses) + `}`
	raw, err := c.call(ctx, MethodTaskUpdate, taskID, json.RawMessage(params))
	if err != nil {
		return nil, err
	}
	if err := checkTaskAck(raw); err != nil {
		return nil, err
	}
	return raw, nil
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
	if err := checkTaskAck(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// checkTaskAck validates the tasks/update and tasks/cancel acknowledgement:
// a JSON object whose resultType is complete.
func checkTaskAck(raw json.RawMessage) error {
	var view struct {
		ResultType string `json:"resultType"`
	}
	if !IsJSONObject(raw) || json.Unmarshal(raw, &view) != nil || view.ResultType != resultTypeComplete {
		return newError(KindProtocol, 0, fmt.Errorf("task acknowledgement is not a complete result"))
	}
	return nil
}

// decodeTaskState projects and validates one detailed task state against
// the pinned state contract. Unknown states, malformed common fields, and
// cross-state payloads fail closed.
//
// expectComplete selects the envelope contract: a tasks/get result must
// carry resultType "complete"; a notification payload must not carry
// resultType at all.
func decodeTaskState(raw json.RawMessage, expectedTaskID string, expectComplete bool) (*TaskState, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, newError(KindProtocol, 0, fmt.Errorf("task state is not a JSON object"))
	}
	if expectComplete {
		rt, ok := stringMember(members, "resultType")
		if !ok || rt != "complete" {
			return nil, newError(KindProtocol, 0, fmt.Errorf("tasks/get result must carry resultType complete"))
		}
	} else if hasMember(members, "resultType") {
		return nil, newError(KindProtocol, 0, fmt.Errorf("task notification carries a resultType"))
	}
	taskID, _ := stringMember(members, "taskId")
	if taskID == "" || (expectedTaskID != "" && taskID != expectedTaskID) {
		return nil, newError(KindProtocol, 0, fmt.Errorf("task state id mismatch"))
	}
	status, _ := stringMember(members, "status")
	if !ValidTaskStatus(status) {
		return nil, newError(KindProtocol, 0, fmt.Errorf("unknown task status %q", status))
	}
	for _, field := range []string{"createdAt", "lastUpdatedAt"} {
		if v, ok := stringMember(members, field); ok {
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				return nil, newError(KindProtocol, 0, fmt.Errorf("task state %s is not an RFC3339 timestamp", field))
			}
		}
	}
	for _, field := range []string{"ttlMs", "pollIntervalMs"} {
		if v, ok := intMember(members, field); ok && v < 0 {
			return nil, newError(KindProtocol, 0, fmt.Errorf("task state %s is negative", field))
		}
	}
	result, hasResult := members["result"]
	failure, hasFailure := members["error"]
	inputRequests, hasInputRequests := members["inputRequests"]
	if err := checkStatePayloads(status, hasResult, result, hasFailure, failure, hasInputRequests, inputRequests); err != nil {
		return nil, err
	}
	var view struct {
		ResultType     string
		StatusMessage  string
		CreatedAt      string
		LastUpdatedAt  string
		TTLMs          int64
		PollIntervalMs int64
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, newError(KindProtocol, 0, fmt.Errorf("task state carries an unsafe integer literal"))
	}
	return &TaskState{
		Raw:            raw,
		ResultType:     view.ResultType,
		TaskID:         taskID,
		Status:         status,
		StatusMessage:  view.StatusMessage,
		CreatedAt:      view.CreatedAt,
		LastUpdatedAt:  view.LastUpdatedAt,
		TTLMs:          view.TTLMs,
		PollIntervalMs: view.PollIntervalMs,
		Result:         result,
		Error:          failure,
		InputRequests:  inputRequests,
	}, nil
}

// checkStatePayloads enforces the state-specific payload contract: a
// completed state carries a valid CallToolResult, a failed state carries an
// error object, an input_required state carries an input-request object, and
// no other state carries any payload. An explicit JSON null is a declared
// payload: it satisfies neither the required shape (it is not a value) nor
// the forbidden rule (it is present at all).
func checkStatePayloads(status string, hasResult bool, result json.RawMessage, hasFailure bool, failure json.RawMessage, hasInputRequests bool, inputRequests json.RawMessage) error {
	isNull := func(raw json.RawMessage) bool { return string(raw) == "null" }
	switch status {
	case TaskCompleted:
		if !hasResult || isNull(result) {
			return newError(KindProtocol, 0, fmt.Errorf("completed task state has no result"))
		}
		if err := checkCallToolResult(result); err != nil {
			return err
		}
	case TaskFailed:
		if !hasFailure || isNull(failure) || !IsJSONObject(failure) {
			return newError(KindProtocol, 0, fmt.Errorf("failed task state has no error object"))
		}
	case TaskInputRequired:
		if !hasInputRequests || isNull(inputRequests) || !IsJSONObject(inputRequests) {
			return newError(KindProtocol, 0, fmt.Errorf("input_required task state has no input request object"))
		}
	default:
		if hasResult || hasFailure || hasInputRequests {
			return newError(KindProtocol, 0, fmt.Errorf("non-terminal task state carries a payload"))
		}
	}
	if (hasResult && hasFailure) || (hasResult && hasInputRequests) || (hasFailure && hasInputRequests) {
		return newError(KindProtocol, 0, fmt.Errorf("task state carries conflicting payloads"))
	}
	return nil
}

// checkCallToolResult validates the nested terminal CallToolResult: a JSON
// object with resultType complete, a content array, and a boolean isError.
func checkCallToolResult(raw json.RawMessage) error {
	if !IsJSONObject(raw) {
		return newError(KindProtocol, 0, fmt.Errorf("completed task result is not a CallToolResult object"))
	}
	var view struct {
		ResultType string          `json:"resultType"`
		Content    json.RawMessage `json:"content"`
		IsError    *bool           `json:"isError"`
	}
	if json.Unmarshal(raw, &view) != nil {
		return newError(KindProtocol, 0, fmt.Errorf("decode completed task result"))
	}
	if view.ResultType != resultTypeComplete {
		return newError(KindProtocol, 0, fmt.Errorf("completed task result has resultType %q", view.ResultType))
	}
	if !IsJSONArray(view.Content) {
		return newError(KindProtocol, 0, fmt.Errorf("completed task result has no content array"))
	}
	if view.IsError == nil {
		return newError(KindProtocol, 0, fmt.Errorf("completed task result has no isError flag"))
	}
	return nil
}

// hasMember reports whether the object declares the named member, including
// an explicit null.
func hasMember(members map[string]json.RawMessage, key string) bool {
	_, ok := members[key]
	return ok
}

// stringMember returns one string member, ok=false when absent or not a
// string.
func stringMember(members map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := members[key]
	if !ok {
		return "", false
	}
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	return v, true
}

// intMember returns one integer member, ok=false when absent or not an
// integer.
func intMember(members map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := members[key]
	if !ok {
		return 0, false
	}
	var v int64
	if json.Unmarshal(raw, &v) != nil {
		return 0, false
	}
	return v, true
}

// IsJSONArray reports whether raw is a top-level JSON array.
func IsJSONArray(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")
}

// jsonString renders a string as a JSON literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
