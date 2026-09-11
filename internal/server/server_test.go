package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/catalog"
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
		Bounds:       profile.Bounds{ProtocolMin: "2025-03-26", ProtocolMax: "2025-11-25"},
		State:        profile.StateRefs{Database: "default", Credentials: "default"},
		Operations:   []catalog.Descriptor{op},
	}
	if err := p.Validate(p.Name); err != nil {
		panic(err)
	}
	return p
}

func connectTestServer(t *testing.T, p *profile.Profile) *mcp.ClientSession {
	t.Helper()

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := New(p, "test").Connect(ctx, serverTransport, nil)
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
	enum, _ := toolProp["enum"].([]any)
	names := make([]string, 0, len(enum))
	for _, item := range enum {
		names = append(names, item.(string))
	}
	if !slices.Equal(names, []string{"message"}) {
		t.Fatalf("tool enum = %v, want [message]", names)
	}

	if !strings.Contains(submitTool.Description, "message") ||
		!strings.Contains(submitTool.Description, "Send one message to Tama.") {
		t.Fatalf("submit description missing operation signature: %q", submitTool.Description)
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
