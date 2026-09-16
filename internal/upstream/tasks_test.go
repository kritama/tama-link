package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

const taskWorkingFixture = `{
  "resultType": "complete",
  "taskId": "d59d7f2a-933e-44f4-8c28-4d28e9f0d937",
  "status": "working",
  "statusMessage": "Indexing memories",
  "createdAt": "2026-09-11T10:00:00Z",
  "lastUpdatedAt": "2026-09-11T10:00:01Z",
  "ttlMs": 86400000,
  "pollIntervalMs": 1000
}`

const taskCompletedFixture = `{
  "resultType": "complete",
  "taskId": "d59d7f2a-933e-44f4-8c28-4d28e9f0d937",
  "status": "completed",
  "statusMessage": "The message completed successfully.",
  "createdAt": "2026-09-11T10:00:00Z",
  "lastUpdatedAt": "2026-09-11T10:00:05Z",
  "ttlMs": 86400000,
  "pollIntervalMs": 1000,
  "result": {
    "resultType": "complete",
    "content": [{"type": "text", "text": "The project foundation is complete."}],
    "structuredContent": {"status": "completed"},
    "isError": false
  }
}`

const taskFailedFixture = `{
  "resultType": "complete",
  "taskId": "d59d7f2a-933e-44f4-8c28-4d28e9f0d937",
  "status": "failed",
  "statusMessage": "The message failed.",
  "createdAt": "2026-09-11T10:00:00Z",
  "lastUpdatedAt": "2026-09-11T10:00:05Z",
  "ttlMs": 86400000,
  "pollIntervalMs": 1000,
  "error": {"code": "upstream_failure", "message": "Bounded failure detail."}
}`

const taskInputRequiredFixture = `{
  "resultType": "complete",
  "taskId": "d59d7f2a-933e-44f4-8c28-4d28e9f0d937",
  "status": "input_required",
  "statusMessage": "Approval needed.",
  "createdAt": "2026-09-11T10:00:00Z",
  "lastUpdatedAt": "2026-09-11T10:00:05Z",
  "ttlMs": 86400000,
  "pollIntervalMs": 1000,
  "inputRequests": {"approval": {"mode": "elicitation", "schema": {"type": "object"}}}
}`

// TestTaskGet covers status retrieval for every TamaMCP task state and the
// state-specific payload contract.
func TestTaskGet(t *testing.T) {
	cases := []struct {
		name     string
		fixture  string
		status   string
		terminal bool
	}{
		{"working", taskWorkingFixture, TaskWorking, false},
		{"ttl at the maximum safe integer", `{"resultType":"complete","taskId":"d59d7f2a-933e-44f4-8c28-4d28e9f0d937","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":9007199254740991}`, TaskWorking, false},
		{"completed", taskCompletedFixture, TaskCompleted, true},
		{"failed", taskFailedFixture, TaskFailed, true},
		{"input required", taskInputRequiredFixture, TaskInputRequired, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
				return 200, "application/json", jsonReply(rec.BodyID, tc.fixture)
			})
			client := newTestClient(t, ts)
			state, err := client.TaskGet(context.Background(), "d59d7f2a-933e-44f4-8c28-4d28e9f0d937")
			if err != nil {
				t.Fatalf("TaskGet: %v", err)
			}
			assertRequestWire(t, ts.requests[0], MethodTaskGet, "d59d7f2a-933e-44f4-8c28-4d28e9f0d937")
			if state.Status != tc.status {
				t.Errorf("status = %q, want %q", state.Status, tc.status)
			}
			if state.IsTerminal() != tc.terminal {
				t.Errorf("terminal = %v, want %v", state.IsTerminal(), tc.terminal)
			}
			if state.TaskID != "d59d7f2a-933e-44f4-8c28-4d28e9f0d937" {
				t.Errorf("taskId = %q", state.TaskID)
			}
			assertNoLegacyMethods(t, ts)
		})
	}
}

// TestTaskGetTerminalResult proves the terminal CallToolResult is captured
// losslessly from the detailed tasks/get state, without any tasks/result
// call.
func TestTaskGetTerminalResult(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, taskCompletedFixture)
	})
	client := newTestClient(t, ts)
	state, err := client.TaskGet(context.Background(), "d59d7f2a-933e-44f4-8c28-4d28e9f0d937")
	if err != nil {
		t.Fatalf("TaskGet: %v", err)
	}
	if len(state.Result) == 0 {
		t.Fatal("completed state has no result payload")
	}
	if !containsRawLiteral(state.Result, `"isError": false`) {
		t.Errorf("result lost isError flag: %s", state.Result)
	}
	if !containsRawLiteral(state.Result, `"status": "completed"`) {
		t.Errorf("result lost structured content: %s", state.Result)
	}
	if len(ts.requests) != 1 {
		t.Fatalf("expected exactly one upstream request, got %d", len(ts.requests))
	}
	assertNoLegacyMethods(t, ts)
}

// TestTaskGetRejectedStates covers fail-closed decoding: unknown status,
// mismatched task ID, and cross-state payloads.
func TestTaskGetRejectedStates(t *testing.T) {
	taskID := "d59d7f2a-933e-44f4-8c28-4d28e9f0d937"
	cases := []struct {
		name    string
		fixture string
	}{
		{"unknown status", `{"taskId":"` + taskID + `","status":"paused"}`},
		{"id mismatch", `{"taskId":"other","status":"working"}`},
		{"missing id", `{"status":"working"}`},
		{"completed without result", `{"taskId":"` + taskID + `","status":"completed"}`},
		{"failed without error", `{"taskId":"` + taskID + `","status":"failed"}`},
		{"input required without requests", `{"taskId":"` + taskID + `","status":"input_required"}`},
		{"working with result payload", `{"taskId":"` + taskID + `","status":"working","result":{"content":[]}}`},
		{"conflicting payloads", `{"taskId":"` + taskID + `","status":"completed","result":{},"error":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
				return 200, "application/json", jsonReply(rec.BodyID, tc.fixture)
			})
			client := newTestClient(t, ts)
			_, err := client.TaskGet(context.Background(), taskID)
			if err == nil {
				t.Fatal("rejected state accepted")
			}
			var uerr *Error
			if !errors.As(err, &uerr) || uerr.Kind != KindProtocol {
				t.Fatalf("err = %v, want protocol kind", err)
			}
		})
	}
}

// TestTaskGetNotFound covers the owner-indistinguishable lookup failure:
// unknown and unauthorized tasks present the same classified error.
func TestTaskGetNotFound(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 404, "application/json", jsonErrorReply(rec.BodyID, -32601, "Task not found")
	})
	client := newTestClient(t, ts)
	_, err := client.TaskGet(context.Background(), "missing")
	var uerr *Error
	if !errors.As(err, &uerr) || uerr.Kind != KindProtocol || uerr.Code != -32601 {
		t.Fatalf("err = %+v, want protocol -32601", err)
	}
}

// TestTaskGetMalformedFixtures covers the malformed state contract: JSON
// null payloads, missing resultType, invalid timestamps, unsafe TTLs, and
// invalid nested CallToolResults all fail closed with protocol errors.
func TestTaskGetMalformedFixtures(t *testing.T) {
	taskID := "d59d7f2a-933e-44f4-8c28-4d28e9f0d937"
	cases := []struct {
		name    string
		fixture string
	}{
		{"completed with null result", `{"resultType":"complete","taskId":"` + taskID + `","status":"completed","result":null}`},
		{"failed with null error", `{"resultType":"complete","taskId":"` + taskID + `","status":"failed","error":null}`},
		{"input required with null requests", `{"resultType":"complete","taskId":"` + taskID + `","status":"input_required","inputRequests":null}`},
		{"working with explicit null payload", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","result":null}`},
		{"missing resultType", `{"taskId":"` + taskID + `","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":86400000,"pollIntervalMs":1000}`},
		{"invalid created timestamp", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":"yesterday","lastUpdatedAt":"2026-09-11T10:00:00Z","ttlMs":86400000,"pollIntervalMs":1000}`},
		{"invalid updated timestamp", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":42,"ttlMs":86400000,"pollIntervalMs":1000}`},
		{"missing created timestamp", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":86400000,"pollIntervalMs":1000}`},
		{"missing updated timestamp", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":"2026-09-11T10:00:00Z","ttlMs":86400000,"pollIntervalMs":1000}`},
		{"null created timestamp", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":null,"lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":86400000,"pollIntervalMs":1000}`},
		{"missing ttl", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","pollIntervalMs":1000}`},
		{"null ttl", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":null,"pollIntervalMs":1000}`},
		{"null poll interval", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":86400000,"pollIntervalMs":null}`},
		{"ttl above the maximum safe integer", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":9007199254740992,"pollIntervalMs":1000}`},
		{"poll interval above the maximum safe integer", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":86400000,"pollIntervalMs":9007199254740992}`},
		{"negative ttl", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":-1,"pollIntervalMs":1000}`},
		{"negative poll interval", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":86400000,"pollIntervalMs":-5}`},
		{"result with wrong resultType", `{"resultType":"complete","taskId":"` + taskID + `","status":"completed","result":{"resultType":"task","content":[],"isError":false}}`},
		{"result without content array", `{"resultType":"complete","taskId":"` + taskID + `","status":"completed","result":{"resultType":"complete","content":{"type":"text"},"isError":false}}`},
		{"result without isError", `{"resultType":"complete","taskId":"` + taskID + `","status":"completed","result":{"resultType":"complete","content":[]}}`},
		{"result is an array", `{"resultType":"complete","taskId":"` + taskID + `","status":"completed","result":[{"content":[]}]}`},
		{"failed error is a string", `{"resultType":"complete","taskId":"` + taskID + `","status":"failed","error":"boom"}`},
		{"input requests is an array", `{"resultType":"complete","taskId":"` + taskID + `","status":"input_required","inputRequests":[1]}`},
		{"unsafe integer ttl", `{"resultType":"complete","taskId":"` + taskID + `","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":92233720368547758080,"pollIntervalMs":1000}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
				return 200, "application/json", jsonReply(rec.BodyID, tc.fixture)
			})
			client := newTestClient(t, ts)
			_, err := client.TaskGet(context.Background(), taskID)
			if err == nil {
				t.Fatal("malformed state accepted")
			}
			var uerr *Error
			if !errors.As(err, &uerr) || uerr.Kind != KindProtocol {
				t.Fatalf("err = %v, want protocol kind", err)
			}
		})
	}
}

// TestTaskUpdateAckShape proves the tasks/update acknowledgement must be the
// documented complete-result shape, and the same for tasks/cancel.
func TestTaskUpdateAckShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		call func(c *Client) error
	}{
		{"update resultType task", `{"resultType":"task"}`, func(c *Client) error {
			_, err := c.TaskUpdate(context.Background(), "task-1", json.RawMessage(`{}`))
			return err
		}},
		{"update empty object", `{}`, func(c *Client) error {
			_, err := c.TaskUpdate(context.Background(), "task-1", json.RawMessage(`{}`))
			return err
		}},
		{"cancel empty object", `{}`, func(c *Client) error {
			_, err := c.TaskCancel(context.Background(), "task-1")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
				return 200, "application/json", jsonReply(rec.BodyID, tc.body)
			})
			client := newTestClient(t, ts)
			err := tc.call(client)
			if err == nil {
				t.Fatal("invalid acknowledgement accepted")
			}
			var uerr *Error
			if !errors.As(err, &uerr) || uerr.Kind != KindProtocol {
				t.Fatalf("err = %v, want protocol kind", err)
			}
		})
	}
}

// TestTaskUpdate covers input-response submission with the task ID as
// Mcp-Name and the responses object verbatim.
func TestTaskUpdate(t *testing.T) {
	var sent json.RawMessage
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		sent = rec.Params["inputResponses"]
		return 200, "application/json", jsonReply(rec.BodyID, `{"resultType":"complete"}`)
	})
	client := newTestClient(t, ts)
	responses := json.RawMessage(`{"approval":{"action":"accept","content":{"approved":true}}}`)
	raw, err := client.TaskUpdate(context.Background(), "task-1", responses)
	if err != nil {
		t.Fatalf("TaskUpdate: %v", err)
	}
	assertRequestWire(t, ts.requests[0], MethodTaskUpdate, "task-1")
	if string(sent) != string(responses) {
		t.Errorf("sent responses %s, want %s", sent, responses)
	}
	if string(raw) != `{"resultType":"complete"}` {
		t.Errorf("ack = %s", raw)
	}
}

// TestTaskUpdateValidation covers client-side parameter checks.
func TestTaskUpdateValidation(t *testing.T) {
	client := newTestClient(t, newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, `{}`)
	}))
	ctx := context.Background()
	if _, err := client.TaskUpdate(ctx, "", json.RawMessage(`{}`)); err == nil {
		t.Error("empty task id accepted")
	}
	if _, err := client.TaskUpdate(ctx, "task-1", json.RawMessage(`[1]`)); err == nil {
		t.Error("non-object responses accepted")
	}
}

// TestTaskCancel covers cooperative cancellation intent.
func TestTaskCancel(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, `{"resultType":"complete"}`)
	})
	client := newTestClient(t, ts)
	if _, err := client.TaskCancel(context.Background(), "task-1"); err != nil {
		t.Fatalf("TaskCancel: %v", err)
	}
	assertRequestWire(t, ts.requests[0], MethodTaskCancel, "task-1")
}

// TestTaskMethodValidation covers empty task IDs on get and cancel.
func TestTaskMethodValidation(t *testing.T) {
	client := newTestClient(t, newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, `{}`)
	}))
	ctx := context.Background()
	if _, err := client.TaskGet(ctx, ""); err == nil {
		t.Error("empty task id accepted by TaskGet")
	}
	if _, err := client.TaskCancel(ctx, ""); err == nil {
		t.Error("empty task id accepted by TaskCancel")
	}
}

// TestValidTaskStatus covers the status vocabulary.
func TestValidTaskStatus(t *testing.T) {
	for _, status := range []string{TaskWorking, TaskInputRequired, TaskCompleted, TaskFailed, TaskCancelled} {
		if !ValidTaskStatus(status) {
			t.Errorf("status %q rejected", status)
		}
	}
	for _, status := range []string{"", "pending", "succeeded", "WORKING"} {
		if ValidTaskStatus(status) {
			t.Errorf("status %q accepted", status)
		}
	}
}
