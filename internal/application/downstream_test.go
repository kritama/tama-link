package application

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/server"
)

// These tests drive the real application through the downstream MCP server.
// The handlers stay thin: they decode, call the profile service, and encode.

func TestDownstreamListsSubmitAndAwait(t *testing.T) {
	t.Parallel()

	session := connectDownstream(t, newFakeTama(t))
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(result.Tools))
	var awaitSchema map[string]any
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
		if tool.Name == contract.ToolAwait {
			awaitSchema, _ = tool.InputSchema.(map[string]any)
		}
		if tool.Name != contract.ToolSubmit {
			continue
		}
		schema, _ := tool.InputSchema.(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		toolProp, _ := properties["tool"].(map[string]any)
		if _, hasEnum := toolProp["enum"]; hasEnum {
			t.Fatalf("submit schema constrains tool: %v", toolProp)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{contract.ToolAwait, contract.ToolSubmit}) {
		t.Fatalf("tools = %v", names)
	}
	properties, _ := awaitSchema["properties"].(map[string]any)
	if _, ok := properties["input_responses"]; !ok {
		t.Fatalf("await schema = %v", awaitSchema)
	}
	instructions := session.InitializeResult().Instructions
	if !strings.Contains(instructions, "submit") || !strings.Contains(instructions, fixtureInstructions) {
		t.Fatalf("instructions = %q", instructions)
	}
}

func TestDownstreamSystemSubmitAwaitAndReplay(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	session := connectDownstream(t, f)
	first := callDownstream(t, session, contract.ToolSubmit, systemSubmitBody("sys-1", "unit"))
	var accepted contract.SubmitOutput
	decodeDownstream(t, first, &accepted)
	if accepted.SubmissionID == "" || accepted.Status != contract.StatusAccepted || accepted.NextPollMS == 0 {
		t.Fatalf("submit = %+v", accepted)
	}

	replay := callDownstream(t, session, contract.ToolSubmit, systemSubmitBody("sys-1", "unit"))
	var again contract.SubmitOutput
	decodeDownstream(t, replay, &again)
	if again.SubmissionID != accepted.SubmissionID {
		t.Fatalf("retry id = %s, want %s", again.SubmissionID, accepted.SubmissionID)
	}

	conflict := callDownstream(t, session, contract.ToolSubmit, systemSubmitBody("sys-1", "changed"))
	if !conflict.IsError || downstreamCode(t, conflict) != contract.CodeIdempotencyConflict {
		t.Fatalf("conflict = %+v", conflict.StructuredContent)
	}

	terminal := awaitDownstream(t, session, accepted.SubmissionID, 5000, "")
	if !terminal.Terminal || terminal.Status != contract.StatusCompleted || terminal.Result == nil || terminal.NextPollMS != 0 {
		t.Fatalf("terminal await = %+v", terminal)
	}
	retained := awaitDownstream(t, session, accepted.SubmissionID, 1, terminal.Cursor)
	if !retained.Terminal || retained.Result == nil || len(retained.Events) != 0 {
		t.Fatalf("retained await = %+v", retained)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("tools/call = %d, want 1", f.calls.Load())
	}
}

func TestDownstreamAppInputResponses(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		extra := `"inputRequests":{"approval":{"mode":"elicitation"},"note":{"mode":"elicitation"}}`
		return http.StatusOK, taskState("input_required", "2026-09-11T10:00:04Z", extra)
	}
	session := connectTaskDownstream(t, up)
	accepted := callDownstream(t, session, contract.ToolSubmit, `{"tool":"message","client_request_id":"app-1","arguments":{"message":"hello"}}`)
	var submitted contract.SubmitOutput
	decodeDownstream(t, accepted, &submitted)

	var pending contract.AwaitOutput
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pending = awaitDownstream(t, session, submitted.SubmissionID, 20, "")
		if pending.Status == contract.StatusInputRequired {
			break
		}
	}
	if pending.Status != contract.StatusInputRequired || !strings.Contains(string(pending.InputRequests), `"approval"`) {
		t.Fatalf("input_required await = %+v", pending)
	}
	if pending.NextPollMS == 0 {
		t.Fatal("pending await omitted polling guidance")
	}

	partial := callDownstream(t, session, contract.ToolAwait, awaitBody(submitted.SubmissionID, 1, "", `{"approval":{"action":"accept"}}`))
	if partial.IsError {
		t.Fatalf("partial response: %v", partial.StructuredContent)
	}
	if up.updateCount() != 1 || strings.Contains(up.updateBody(), `"note"`) {
		t.Fatalf("partial update = %s count %d", up.updateBody(), up.updateCount())
	}
	exact := callDownstream(t, session, contract.ToolAwait, awaitBody(submitted.SubmissionID, 1, "", `{"approval":{"action":"accept"}}`))
	if exact.IsError || up.updateCount() != 1 {
		t.Fatalf("exact replay updates = %d err %v", up.updateCount(), exact.StructuredContent)
	}
	conflict := callDownstream(t, session, contract.ToolAwait, awaitBody(submitted.SubmissionID, 1, "", `{"approval":{"action":"reject"}}`))
	if !conflict.IsError || downstreamCode(t, conflict) != contract.CodeIdempotencyConflict {
		t.Fatalf("changed response = %v", conflict.StructuredContent)
	}
	stale := callDownstream(t, session, contract.ToolAwait, awaitBody(submitted.SubmissionID, 1, "", `{"missing":{"action":"accept"}}`))
	if !stale.IsError || downstreamCode(t, stale) != contract.CodeInvalidRequest {
		t.Fatalf("stale response = %v", stale.StructuredContent)
	}
}

func TestDownstreamAwaitCursorTimeoutCancellationAndErrors(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		return http.StatusOK, taskState("working", "2026-09-11T10:00:02Z", "")
	}
	session := connectTaskDownstream(t, up)
	accepted := callDownstream(t, session, contract.ToolSubmit, `{"tool":"message","client_request_id":"run-1","arguments":{"message":"hello"}}`)
	var submitted contract.SubmitOutput
	decodeDownstream(t, accepted, &submitted)

	var running contract.AwaitOutput
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		running = awaitDownstream(t, session, submitted.SubmissionID, 20, "")
		if running.Status == contract.StatusRunning {
			break
		}
	}
	if running.Status != contract.StatusRunning || running.Terminal || running.Cursor == "" {
		t.Fatalf("running await = %+v", running)
	}
	timedOut := awaitDownstream(t, session, submitted.SubmissionID, 1, running.Cursor)
	if timedOut.Terminal || timedOut.Status != contract.StatusRunning {
		t.Fatalf("timeout await = %+v", timedOut)
	}
	previous, err := strconv.ParseInt(running.Cursor, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range timedOut.Events {
		if event.Sequence <= previous {
			t.Fatalf("cursor %s replayed event %d", running.Cursor, event.Sequence)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      contract.ToolAwait,
		Arguments: json.RawMessage(awaitBody(submitted.SubmissionID, 5000, "", "")),
	})
	if time.Since(started) > 2*time.Second {
		t.Fatalf("cancelled await took %s", time.Since(started))
	}
	if err == nil && result != nil && result.IsError {
		t.Fatalf("cancelled await became a tool error: %v", result.StructuredContent)
	}
	if up.cancelCount() != 0 {
		t.Fatalf("cancellation called tasks/cancel %d times", up.cancelCount())
	}

	missing := callDownstream(t, session, contract.ToolAwait, `{"timeout_ms":1}`)
	if !missing.IsError || downstreamCode(t, missing) != contract.CodeInvalidRequest {
		t.Fatalf("missing submission_id = %v", missing.StructuredContent)
	}
	unknown := callDownstream(t, session, contract.ToolAwait, `{"submission_id":"missing","timeout_ms":1}`)
	if !unknown.IsError || downstreamCode(t, unknown) != contract.CodeSubmissionNotFound {
		t.Fatalf("unknown submission = %v", unknown.StructuredContent)
	}
	badCursor := callDownstream(t, session, contract.ToolAwait, awaitBody(submitted.SubmissionID, 1, "nope", ""))
	if !badCursor.IsError || downstreamCode(t, badCursor) != contract.CodeInvalidRequest {
		t.Fatalf("bad cursor = %v", badCursor.StructuredContent)
	}
}

func connectDownstream(t *testing.T, f *fakeTama) *mcp.ClientSession {
	t.Helper()
	svc, _, _ := fixtureApp(t, f)
	return connectApp(t, svc)
}

func connectTaskDownstream(t *testing.T, up *taskUpstream) *mcp.ClientSession {
	t.Helper()
	svc, _ := taskApp(t, up)
	return connectApp(t, svc)
}

func connectApp(t *testing.T, svc *Service) *mcp.ClientSession {
	t.Helper()
	srv := server.New(svc.profile, "test", svc)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "downstream-test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callDownstream(t *testing.T, session *mcp.ClientSession, name, body string) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: json.RawMessage(body),
	})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return result
}

func awaitDownstream(t *testing.T, session *mcp.ClientSession, id string, timeout int, cursor string) contract.AwaitOutput {
	t.Helper()
	result := callDownstream(t, session, contract.ToolAwait, awaitBody(id, timeout, cursor, ""))
	if result.IsError {
		t.Fatalf("await: %v", result.StructuredContent)
	}
	var out contract.AwaitOutput
	decodeDownstream(t, result, &out)
	return out
}

func awaitBody(id string, timeout int, cursor, responses string) string {
	body := map[string]any{"submission_id": id, "timeout_ms": timeout}
	if cursor != "" {
		body["cursor"] = cursor
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	if responses == "" {
		return string(encoded)
	}
	return strings.TrimSuffix(string(encoded), "}") + `,"input_responses":` + responses + "}"
}

func systemSubmitBody(id, detail string) string {
	return `{"tool":"status","client_request_id":"` + id + `","arguments":{"detail":"` + detail + `"},"client_context":{"thread_id":"thread-1"}}`
}

func decodeDownstream(t *testing.T, result *mcp.CallToolResult, out any) {
	t.Helper()
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("encode structured content: %v", err)
	}
	if err := json.Unmarshal(encoded, out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
}

func downstreamCode(t *testing.T, result *mcp.CallToolResult) contract.Code {
	t.Helper()
	var out contract.ErrorOutput
	decodeDownstream(t, result, &out)
	if out.Error == nil {
		t.Fatalf("missing error payload: %v", result.StructuredContent)
	}
	return out.Error.Code
}
