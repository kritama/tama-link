package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/profile"
)

func testProfile() *profile.Profile {
	op := catalog.Descriptor{
		Name:        "message",
		Title:       "Message",
		Description: "Send one message to Tama.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
		TaskSupport: catalog.TaskSupportRequired,
		Strategy:    catalog.StrategyUpstreamTask,
	}
	digest, err := op.ComputeDigest()
	if err != nil {
		panic(err)
	}
	op.Digest = digest

	p := &profile.Profile{
		Version:      profile.SchemaVersion,
		Name:         profile.Name("tama-app"),
		Origin:       "https://tama.example",
		Endpoint:     "https://tama.example/mcp/app",
		Issuer:       "https://auth.example",
		Instructions: "Pinned upstream instructions.",
		Bounds:       profile.Bounds{ProtocolMin: "2026-07-28", ProtocolMax: "2026-07-28"},
		State:        profile.StateRefs{Database: "default", Credentials: "default"},
		Operations:   []catalog.Descriptor{op},
		Scopes:       []string{"mcp.message"},
	}
	if err := p.Validate(p.Name); err != nil {
		panic(err)
	}
	return p
}

func connectTestServer(t *testing.T, p *profile.Profile) *mcp.ClientSession {
	t.Helper()
	srv, err := New(p, "test", &fakeApp{})
	if err != nil {
		t.Fatal(err)
	}
	return connectServer(t, srv)
}

func mustServer(t *testing.T, p *profile.Profile, app App) *mcp.Server {
	t.Helper()
	srv, err := New(p, "test", app)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func connectServer(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "tama-link-test", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}

func TestSubmitPreservesJSONNumbersAcrossMCPBoundary(t *testing.T) {
	t.Parallel()

	var captured json.RawMessage
	operation := func(
		_ context.Context,
		_ *mcp.CallToolRequest,
		input contract.SubmitInput,
	) (*mcp.CallToolResult, any, error) {
		captured = append(captured[:0], input.Arguments...)
		return &mcp.CallToolResult{}, map[string]any{"accepted": true}, nil
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	server.AddTool(
		&mcp.Tool{Name: contract.ToolSubmit, InputSchema: submitInputSchema()},
		submitHandler(operation),
	)
	client := connectServer(t, server)

	raw := json.RawMessage(`{"tool":"message","arguments":{"identifier":9007199254740993}}`)
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      contract.ToolSubmit,
		Arguments: raw,
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool returned tool error: %+v", result)
	}
	if got, want := string(captured), `{"identifier":9007199254740993}`; got != want {
		t.Fatalf("captured arguments = %s, want %s", got, want)
	}
}

func TestServerRequiresApplication(t *testing.T) {
	t.Parallel()

	if _, err := New(testProfile(), "test", nil); err == nil {
		t.Fatal("nil application constructed a server")
	}
}

func TestServerExposesOnlySubmitAndAwait(t *testing.T) {
	t.Parallel()

	clientSession := connectTestServer(t, testProfile())

	result, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)

	want := []string{"await", "submit"}
	if !slices.Equal(names, want) {
		t.Fatalf("tool names = %v, want %v", names, want)
	}
}

func TestServerProjectsProfileCatalog(t *testing.T) {
	t.Parallel()

	clientSession := connectTestServer(t, testProfile())
	ctx := context.Background()

	result, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(result.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(result.Tools))
	}
	submitTool := result.Tools[0]
	if submitTool.Name == "await" {
		submitTool = result.Tools[1]
	}

	schema, ok := submitTool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("submit input schema type = %T", submitTool.InputSchema)
	}
	properties, _ := schema["properties"].(map[string]any)
	toolProp, _ := properties["tool"].(map[string]any)
	if toolProp == nil || toolProp["type"] != "string" {
		t.Fatalf("submit input schema missing the tool property: %v", properties)
	}
	if _, hasEnum := toolProp["enum"]; hasEnum {
		t.Fatalf("submit input schema constrains tool with an enum; retries of reconciled-away tools must reach the application: %v", toolProp)
	}

	if !strings.Contains(submitTool.Description, "message") ||
		!strings.Contains(submitTool.Description, "Send one message to Tama.") {
		t.Fatalf("submit description missing operation signature: %q", submitTool.Description)
	}
}

// TestServerRoutesRemovedToolRetriesToApp pins the transport's role in
// idempotency recovery: the profile catalog constrains genuinely new work
// at the application, not the transport, so an exact retry whose tool the
// profile later removed still reaches the application, where idempotency
// reconciliation recovers the durable submission before the catalog check
// can fail it.
func TestServerRoutesRemovedToolRetriesToApp(t *testing.T) {
	t.Parallel()

	app := &fakeApp{}
	clientSession := connectServer(t, mustServer(t, testProfile(), app))

	// "removed" is not in the pinned catalog. The transport must not
	// reject it: the retry carries an accepted request's idempotency key.
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: contract.ToolSubmit,
		Arguments: map[string]any{
			"tool":              "removed",
			"client_request_id": "retry-1",
			"arguments":         map[string]any{},
		},
	})
	if err != nil {
		t.Fatalf("CallTool for a removed-tool retry: %v", err)
	}
	if result.IsError {
		t.Fatalf("transport rejected the removed-tool retry: %v", result.StructuredContent)
	}
	if app.lastTool != "removed" {
		t.Fatalf("application saw tool %q, want the removed tool routed to it", app.lastTool)
	}
}

func TestServerComposesInstructions(t *testing.T) {
	t.Parallel()

	clientSession := connectTestServer(t, testProfile())

	instructions := clientSession.InitializeResult().Instructions
	if !strings.Contains(instructions, "submit") || !strings.Contains(instructions, "await") {
		t.Fatalf("instructions missing workflow: %q", instructions)
	}
	if !strings.Contains(instructions, "Pinned upstream instructions.") {
		t.Fatalf("instructions missing pinned upstream copy: %q", instructions)
	}
}

func TestServerWithoutPinnedInstructions(t *testing.T) {
	t.Parallel()

	p := testProfile()
	p.Instructions = ""
	clientSession := connectTestServer(t, p)

	instructions := clientSession.InitializeResult().Instructions
	if instructions != workflowInstructions {
		t.Fatalf("instructions = %q, want workflow only", instructions)
	}
}

// decodeStructured re-encodes a structured-content value and decodes it
// into out, surviving the in-memory transport's any-typed round trip.
func decodeStructured(t *testing.T, content any, out any) {
	t.Helper()
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("encode structured content: %v", err)
	}
	if err := json.Unmarshal(encoded, out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
}

// fakeApp is a deterministic App implementation for handler tests.
type fakeApp struct {
	submitErr *contract.Error
	awaitErr  *contract.Error
	// lastTool records the tool of the last submitted request, so a test
	// can prove which requests the transport routed to the application.
	lastTool string
}

func (f *fakeApp) Submit(_ context.Context, in contract.SubmitInput) (contract.SubmitOutput, *contract.Error) {
	f.lastTool = in.Tool
	if f.submitErr != nil {
		return contract.SubmitOutput{}, f.submitErr
	}
	return contract.SubmitOutput{SubmissionID: "sub-1", Status: contract.StatusAccepted, ClientRequestID: in.ClientRequestID, NextPollMS: 1000}, nil
}

func (f *fakeApp) Await(_ context.Context, in contract.AwaitInput) (contract.AwaitOutput, *contract.Error) {
	if f.awaitErr != nil {
		return contract.AwaitOutput{}, f.awaitErr
	}
	return contract.AwaitOutput{SubmissionID: in.SubmissionID, Tool: "message", Status: contract.StatusCompleted, Terminal: true, Cursor: "1"}, nil
}

func TestServerAppSubmitSuccessAndError(t *testing.T) {
	t.Parallel()

	app := &fakeApp{}
	clientSession := connectServer(t, mustServer(t, testProfile(), app))

	// Success path renders the structured output.
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      contract.ToolSubmit,
		Arguments: map[string]any{"tool": "message", "client_request_id": "r-1", "arguments": map[string]any{"message": "hi"}},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if result.IsError {
		t.Fatalf("submit flagged error: %v", result.StructuredContent)
	}
	var out contract.SubmitOutput
	decodeStructured(t, result.StructuredContent, &out)
	if out.SubmissionID != "sub-1" || out.Status != contract.StatusAccepted {
		t.Fatalf("submit output = %+v", out)
	}

	// App failure renders the stable contract error payload.
	failed := &fakeApp{}
	ce := contract.NewError(contract.CodeInvalidRequest, "bad")
	failed.submitErr = &ce
	failedSession := connectServer(t, mustServer(t, testProfile(), failed))
	failResult, err := failedSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      contract.ToolSubmit,
		Arguments: map[string]any{"tool": "message", "client_request_id": "r-2", "arguments": map[string]any{"message": "hi"}},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !failResult.IsError {
		t.Fatal("expected isError")
	}
	var errOut contract.ErrorOutput
	decodeStructured(t, failResult.StructuredContent, &errOut)
	if errOut.Error == nil || errOut.Error.Code != contract.CodeInvalidRequest {
		t.Fatalf("error output = %+v", errOut)
	}
}

func TestServerAppAwait(t *testing.T) {
	t.Parallel()

	clientSession := connectServer(t, mustServer(t, testProfile(), &fakeApp{}))
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      contract.ToolAwait,
		Arguments: map[string]any{"submission_id": "sub-1", "timeout_ms": 0},
	})
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if result.IsError {
		t.Fatalf("await flagged error: %v", result.StructuredContent)
	}
	var out contract.AwaitOutput
	decodeStructured(t, result.StructuredContent, &out)
	if out.Status != contract.StatusCompleted || !out.Terminal || out.Cursor != "1" {
		t.Fatalf("await output = %+v", out)
	}
}
