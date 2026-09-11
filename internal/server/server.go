// Package server defines Tama Link's client-facing MCP server.
package server

import (
	"context"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/profile"
)

const (
	serverName = "tama-link"

	awaitDescription = "Wait for or inspect a Tama Link submission and return progress or its terminal result."

	submitBaseDescription = "Submit one allowed operation to Tama and return a durable submission identifier without waiting for completion."

	// workflowInstructions is Tama Link's own workflow and local safety
	// constraints, always the first source of server instructions.
	workflowInstructions = "Use submit to start one durable Tama operation, then call await with the returned submission_id until terminal is true. Submit each operation once and reuse client_request_id when retrying so retries stay idempotent."

	notImplementedMessage = "Tama Link's upstream adapter is not implemented in the repository foundation"
)

// New creates a client-facing MCP server for one validated profile with
// exactly the submit and await tools.
func New(p *profile.Profile, buildVersion string) *mcp.Server {
	ops := p.Catalog().Callable()

	instance := mcp.NewServer(
		&mcp.Implementation{Name: serverName, Version: buildVersion},
		&mcp.ServerOptions{Instructions: composeInstructions(p.Instructions)},
	)

	mcp.AddTool(instance, &mcp.Tool{
		Name:        contract.ToolSubmit,
		Description: composeSubmitDescription(ops),
		InputSchema: submitInputSchema(ops.Names()),
	}, submit)

	mcp.AddTool(instance, &mcp.Tool{
		Name:        contract.ToolAwait,
		Description: awaitDescription,
	}, await)

	return instance
}

// composeInstructions joins Tama Link's workflow with the profile's pinned
// upstream instructions as two clearly separated sources.
func composeInstructions(pinned string) string {
	if pinned == "" {
		return workflowInstructions
	}
	return workflowInstructions + "\n\n" + pinned
}

// composeSubmitDescription renders the base description plus the bounded
// deterministic operation signatures.
func composeSubmitDescription(ops catalog.Catalog) string {
	signatures := ops.SubmitDescription()
	if signatures == "" {
		return submitBaseDescription
	}
	return submitBaseDescription + "\n\n" + signatures
}

// submitInputSchema constrains tool to the approved operation names.
func submitInputSchema(names []string) map[string]any {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tool": map[string]any{
				"type":        "string",
				"enum":        sorted,
				"description": "upstream Tama tool name allowed by the selected profile",
			},
			"arguments": map[string]any{
				"type":        "object",
				"description": "arguments for the upstream Tama tool",
			},
			"client_request_id": map[string]any{
				"type":        "string",
				"description": "opaque idempotency key scoped to the selected profile",
			},
			"client_context": map[string]any{
				"type":        "object",
				"description": "client-owned correlation values",
				"properties": map[string]any{
					"thread_id": map[string]any{
						"type":        "string",
						"description": "host conversation identifier supplied by the client",
					},
				},
			},
		},
		"required": []string{"tool"},
	}
}

func submit(_ context.Context, _ *mcp.CallToolRequest, _ contract.SubmitInput) (*mcp.CallToolResult, any, error) {
	notImplemented := contract.NewError(contract.CodeNotImplemented, notImplementedMessage)
	return &mcp.CallToolResult{IsError: true}, contract.ErrorOutput{Error: &notImplemented}, nil
}

func await(_ context.Context, _ *mcp.CallToolRequest, _ contract.AwaitInput) (*mcp.CallToolResult, any, error) {
	notImplemented := contract.NewError(contract.CodeNotImplemented, notImplementedMessage)
	return &mcp.CallToolResult{IsError: true}, contract.ErrorOutput{Error: &notImplemented}, nil
}
