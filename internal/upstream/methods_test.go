package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const discoverFixture = `{
  "resultType": "complete",
  "supportedVersions": ["2026-07-28"],
  "capabilities": {
    "tools": {},
    "extensions": {"io.modelcontextprotocol/tasks": {}}
  },
  "_meta": {"io.modelcontextprotocol/serverInfo": {"name": "tama", "version": "1.0.0"}},
  "instructions": "Send work to Tama and await terminal results.",
  "ttlMs": 0,
  "cacheScope": "private"
}`

// TestDiscover verifies the discovery handshake, header wire agreement, and
// result projection.
func TestDiscover(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, discoverFixture)
	})
	client := newTestClient(t, ts)
	result, err := client.Discover(context.Background(), 0)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	assertRequestWire(t, ts.requests[0], MethodDiscover, "")
	if len(result.SupportedVersions) != 1 || result.SupportedVersions[0] != protocolVersion {
		t.Errorf("supportedVersions = %v", result.SupportedVersions)
	}
	if !result.HasTaskExtension() {
		t.Error("task extension not detected")
	}
	if result.Instructions == "" {
		t.Error("instructions missing")
	}
	if result.ServerInfo == nil || result.ServerInfo.Name != "tama" || result.ServerInfo.Version != "1.0.0" {
		t.Errorf("serverInfo = %+v", result.ServerInfo)
	}
	assertNoLegacyMethods(t, ts)
}

// TestDiscoverNoTaskExtension covers a server without the Tasks extension.
func TestDiscoverNoTaskExtension(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, `{
			"supportedVersions": ["2026-07-28"],
			"capabilities": {"tools": {}}
		}`)
	})
	client := newTestClient(t, ts)
	result, err := client.Discover(context.Background(), 0)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if result.HasTaskExtension() {
		t.Error("task extension reported on a server without it")
	}
}

// TestDiscoverUnusableResults covers fail-closed discovery results.
func TestDiscoverUnusableResults(t *testing.T) {
	cases := []struct {
		name   string
		result string
	}{
		{"no versions", `{"capabilities":{"tools":{}}}`},
		{"invalid server info", `{"supportedVersions":["2026-07-28"],"_meta":{"io.modelcontextprotocol/serverInfo":[1]}}`},
		{"not an object", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
				return 200, "application/json", jsonReply(rec.BodyID, tc.result)
			})
			client := newTestClient(t, ts)
			if _, err := client.Discover(context.Background(), 0); err == nil {
				t.Fatal("unusable discovery result accepted")
			}
		})
	}
}

const toolsFixture = `{
  "resultType": "complete",
  "tools": [
    {
      "name": "message",
      "description": "Send a message to Tama",
      "inputSchema": {
        "type": "object",
        "properties": {"message": {"type": "string", "minLength": 1}, "limit": {"type": "integer"}},
        "required": ["message"],
        "additionalProperties": false
      },
      "outputSchema": {
        "type": "object",
        "properties": {"status": {"type": "string"}},
        "required": ["status"],
        "additionalProperties": false
      }
    },
    {
      "name": "inspect",
      "description": "Inspect Tama state",
      "inputSchema": {"type": "object", "properties": {}, "additionalProperties": false}
    }
  ],
  "ttlMs": 0,
  "cacheScope": "private"
}`

// TestListTools verifies catalog listing preserves raw schema number
// literals, server order, and pagination.
func TestListTools(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, toolsFixture)
	})
	client := newTestClient(t, ts)
	tools, err := client.ListAllTools(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListAllTools: %v", err)
	}
	assertRequestWire(t, ts.requests[0], MethodListTools, "")
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	if tools[0].Name != "message" || tools[1].Name != "inspect" {
		t.Errorf("tool order = %q, %q", tools[0].Name, tools[1].Name)
	}
	// The raw schema must survive untouched for canonical digest comparison.
	if !containsRawLiteral(tools[0].InputSchema, `"integer"`) {
		t.Errorf("inputSchema lost raw schema: %s", tools[0].InputSchema)
	}
	assertNoLegacyMethods(t, ts)
}

// TestListToolsPagination covers cursor following and the page ceiling.
func TestListToolsPagination(t *testing.T) {
	seen := 0
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		seen++
		cursor := ""
		if v := rec.Params["cursor"]; v != nil {
			_ = json.Unmarshal(v, &cursor)
		}
		switch cursor {
		case "":
			return 200, "application/json", jsonReply(rec.BodyID, `{"tools":[{"name":"a","inputSchema":{}}],"nextCursor":"p2"}`)
		case "p2":
			return 200, "application/json", jsonReply(rec.BodyID, `{"tools":[{"name":"b","inputSchema":{}}]}`)
		}
		return 200, "application/json", jsonReply(rec.BodyID, `{"tools":[]}`)
	})
	client := newTestClient(t, ts)
	tools, err := client.ListAllTools(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListAllTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools across pages, want 2", len(tools))
	}
	if seen != 2 {
		t.Fatalf("saw %d pages, want 2", seen)
	}

	// A looping cursor must hit the page ceiling instead of running forever.
	loop := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, `{"tools":[],"nextCursor":"loop"}`)
	})
	loopClient := newTestClient(t, loop)
	if _, err := loopClient.ListAllTools(context.Background(), 0); err == nil {
		t.Fatal("cursor loop was not bounded")
	}
}

const callTaskFixture = `{
  "resultType": "task",
  "taskId": "d59d7f2a-933e-44f4-8c28-4d28e9f0d937",
  "status": "working",
  "statusMessage": "The message is queued for processing.",
  "createdAt": "2026-09-11T10:00:00Z",
  "lastUpdatedAt": "2026-09-11T10:00:00Z",
  "ttlMs": 86400000,
  "pollIntervalMs": 1000
}`

const callCompleteFixture = `{
  "resultType": "complete",
  "content": [{"type": "text", "text": "Tama is available."}],
  "structuredContent": {"status": "available", "n": 9007199254740993},
  "isError": false
}`

// TestCallToolTask covers server-directed task creation and number-literal
// preservation of arguments.
func TestCallToolTask(t *testing.T) {
	var sentArgs json.RawMessage
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		sentArgs = rec.Params["arguments"]
		return 200, "application/json", jsonReply(rec.BodyID, callTaskFixture)
	})
	client := newTestClient(t, ts)
	args := json.RawMessage(`{"message":"Summarize the current project state.","big":9007199254740993}`)
	resp, err := client.CallTool(context.Background(), &CallToolParams{Name: "message", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	assertRequestWire(t, ts.requests[0], MethodCallTool, "message")
	if !resp.IsTask() {
		t.Fatalf("resultType = %q, want task", resp.ResultType)
	}
	if resp.TaskID != "d59d7f2a-933e-44f4-8c28-4d28e9f0d937" {
		t.Errorf("taskId = %q", resp.TaskID)
	}
	if resp.TTLMs != 86400000 || resp.PollIntervalMs != 1000 {
		t.Errorf("ttl/poll = %d/%d", resp.TTLMs, resp.PollIntervalMs)
	}
	if string(sentArgs) != string(args) {
		t.Errorf("sent arguments %s, want %s", sentArgs, args)
	}
	assertNoLegacyMethods(t, ts)
}

// TestCallToolCapabilityOverride proves the per-request capabilities seam:
// one request's _meta triple can declare a different capability set without
// touching the client's default.
func TestCallToolCapabilityOverride(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, callTaskFixture)
	})
	client := newTestClient(t, ts)

	// Default request carries the client's declared capabilities.
	if _, err := client.CallTool(context.Background(), &CallToolParams{Name: "message"}); err != nil {
		t.Fatalf("default CallTool: %v", err)
	}
	if got := string(ts.requests[0].Meta["io.modelcontextprotocol/clientCapabilities"]); !strings.Contains(got, "io.modelcontextprotocol/tasks") {
		t.Errorf("default capabilities = %s", got)
	}

	// The override replaces the capabilities for this one request only.
	override := json.RawMessage(`{}`)
	if _, err := client.CallTool(context.Background(), &CallToolParams{Name: "message", Capabilities: override}); err != nil {
		t.Fatalf("override CallTool: %v", err)
	}
	if got := string(ts.requests[1].Meta["io.modelcontextprotocol/clientCapabilities"]); got != `{}` {
		t.Errorf("override capabilities = %s, want {}", got)
	}

	// The client default is unaffected by the override.
	if _, err := client.CallTool(context.Background(), &CallToolParams{Name: "message"}); err != nil {
		t.Fatalf("post-override CallTool: %v", err)
	}
	if got := string(ts.requests[2].Meta["io.modelcontextprotocol/clientCapabilities"]); !strings.Contains(got, "io.modelcontextprotocol/tasks") {
		t.Errorf("post-override capabilities = %s, want the client default", got)
	}
}

// TestCallToolComplete covers the synchronous result shape with a number
// literal beyond float64 precision kept raw.
func TestCallToolComplete(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, callCompleteFixture)
	})
	client := newTestClient(t, ts)
	resp, err := client.CallTool(context.Background(), &CallToolParams{Name: "inspect"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if resp.IsTask() {
		t.Fatal("complete result reported as task")
	}
	if !containsRawLiteral(resp.Raw, "9007199254740993") {
		t.Errorf("structured content lost number literal: %s", resp.Raw)
	}
}

func containsRawLiteral(raw json.RawMessage, needle string) bool {
	for i := 0; i+len(needle) <= len(raw); i++ {
		if string(raw[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}

// TestCallToolSSE covers the same call answered over SSE.
func TestCallToolSSE(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "text/event-stream",
			": keepalive\n" + sseReply(jsonReply(rec.BodyID, callTaskFixture))
	})
	client := newTestClient(t, ts)
	resp, err := client.CallTool(context.Background(), &CallToolParams{Name: "message"})
	if err != nil {
		t.Fatalf("CallTool over SSE: %v", err)
	}
	if !resp.IsTask() || resp.TaskID == "" {
		t.Fatalf("SSE task response = %+v", resp)
	}
}

// TestCallToolRejectsUnknownResultType fails closed on an unknown shape.
func TestCallToolRejectsUnknownResultType(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, `{"resultType":"weird"}`)
	})
	client := newTestClient(t, ts)
	_, err := client.CallTool(context.Background(), &CallToolParams{Name: "message"})
	if err == nil {
		t.Fatal("unknown resultType accepted")
	}
}

// TestCallToolRejectsInvalidInitialTaskResults proves a task-shaped result
// fails closed on the first response, before any polling begins: the pinned
// TamaMCP profile creates tasks in the working state with the full common
// envelope, so a terminal, unknown, or incomplete initial state is a
// protocol failure.
func TestCallToolRejectsInvalidInitialTaskResults(t *testing.T) {
	for _, tc := range []string{
		`{"resultType":"task","taskId":"t-1","status":"paused","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:00Z","ttlMs":86400000,"pollIntervalMs":1000}`,
		`{"resultType":"task","taskId":"t-1"}`,
		`{"resultType":"task","taskId":"t-1","status":"working"}`,
		`{"resultType":"task","taskId":"t-1","status":"completed","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:00Z","ttlMs":86400000,"pollIntervalMs":1000}`,
		`{"resultType":"task","taskId":"t-1","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:00Z","ttlMs":null}`,
		`{"resultType":"task","taskId":"t-1","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:00Z","ttlMs":9007199254740992,"pollIntervalMs":1000}`,
	} {
		ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
			return 200, "application/json", jsonReply(rec.BodyID, tc)
		})
		client := newTestClient(t, ts)
		_, err := client.CallTool(context.Background(), &CallToolParams{Name: "message"})
		var uerr *Error
		if !errors.As(err, &uerr) || uerr.Kind != KindProtocol {
			t.Fatalf("%s: err = %v, want protocol kind", tc, err)
		}
	}
}

// TestCallToolValidation covers client-side parameter checks.
func TestCallToolValidation(t *testing.T) {
	client := newTestClient(t, newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, callCompleteFixture)
	}))
	ctx := context.Background()
	if _, err := client.CallTool(ctx, nil); err == nil {
		t.Error("nil params accepted")
	}
	if _, err := client.CallTool(ctx, &CallToolParams{Name: ""}); err == nil {
		t.Error("empty tool name accepted")
	}
}
