// Package server defines Tama Link's client-facing MCP server.
package server

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/contract"
)

const (
	serverName = "tama-link"

	instructions = "Use submit to start one durable Tama operation, then call await with the returned submission_id until terminal is true."

	submitDescription = "Submit one allowed operation to Tama and return a durable submission identifier without waiting for completion."
	awaitDescription  = "Wait for or inspect a Tama Link submission and return progress or its terminal result."

	notImplementedMessage = "Tama Link's upstream adapter is not implemented in the repository foundation"
)

// New creates a client-facing MCP server with exactly the submit and await tools.
func New(buildVersion string) *mcp.Server {
	instance := mcp.NewServer(
		&mcp.Implementation{Name: serverName, Version: buildVersion},
		&mcp.ServerOptions{Instructions: instructions},
	)

	mcp.AddTool(instance, &mcp.Tool{
		Name:        contract.ToolSubmit,
		Description: submitDescription,
	}, submit)

	mcp.AddTool(instance, &mcp.Tool{
		Name:        contract.ToolAwait,
		Description: awaitDescription,
	}, await)

	return instance
}

func submit(_ context.Context, _ *mcp.CallToolRequest, _ contract.SubmitInput) (*mcp.CallToolResult, any, error) {
	notImplemented := contract.NewError(contract.CodeNotImplemented, notImplementedMessage)
	return &mcp.CallToolResult{IsError: true}, contract.ErrorOutput{Error: &notImplemented}, nil
}

func await(_ context.Context, _ *mcp.CallToolRequest, _ contract.AwaitInput) (*mcp.CallToolResult, any, error) {
	notImplemented := contract.NewError(contract.CodeNotImplemented, notImplementedMessage)
	return &mcp.CallToolResult{IsError: true}, contract.ErrorOutput{Error: &notImplemented}, nil
}
