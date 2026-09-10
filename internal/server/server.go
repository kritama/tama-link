// Package server defines Tama Link's client-facing MCP server.
package server

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName = "tama-link"

	notImplementedMessage = "Tama Link's upstream adapter is not implemented in the repository foundation"
)

// SubmitInput is the stable client-facing input for the submit tool.
type SubmitInput struct {
	Tool            string         `json:"tool" jsonschema:"upstream Tama tool name allowed by the selected profile"`
	Arguments       map[string]any `json:"arguments,omitempty" jsonschema:"arguments for the upstream Tama tool"`
	ClientRequestID string         `json:"client_request_id,omitempty" jsonschema:"opaque idempotency key scoped to the selected profile"`
}

// AwaitInput is the stable client-facing input for the await tool.
type AwaitInput struct {
	SubmissionID string `json:"submission_id" jsonschema:"opaque Tama Link submission identifier"`
	Cursor       string `json:"cursor,omitempty" jsonschema:"opaque progress cursor returned by an earlier await call"`
	TimeoutMS    int    `json:"timeout_ms,omitempty" jsonschema:"bounded long-poll duration in milliseconds"`
}

// ToolError is the stable error envelope returned by Tama Link tools.
type ToolError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// SubmitOutput is the initial structured output shape for submit.
type SubmitOutput struct {
	Status string     `json:"status"`
	Error  *ToolError `json:"error,omitempty"`
}

// AwaitOutput is the initial structured output shape for await.
type AwaitOutput struct {
	Status   string     `json:"status"`
	Terminal bool       `json:"terminal"`
	Error    *ToolError `json:"error,omitempty"`
}

// New creates a client-facing MCP server with exactly the submit and await tools.
func New(buildVersion string) *mcp.Server {
	instance := mcp.NewServer(
		&mcp.Implementation{Name: serverName, Version: buildVersion},
		&mcp.ServerOptions{
			Instructions: "Use submit to start one durable Tama operation, then call await with the returned submission_id until terminal is true.",
		},
	)

	mcp.AddTool(instance, &mcp.Tool{
		Name:        "submit",
		Description: "Submit one allowed operation to Tama and return a durable submission identifier without waiting for completion.",
	}, submit)

	mcp.AddTool(instance, &mcp.Tool{
		Name:        "await",
		Description: "Wait for or inspect a Tama Link submission and return progress or its terminal result.",
	}, await)

	return instance
}

func submit(_ context.Context, _ *mcp.CallToolRequest, _ SubmitInput) (*mcp.CallToolResult, SubmitOutput, error) {
	return &mcp.CallToolResult{IsError: true}, SubmitOutput{
		Status: "not_implemented",
		Error: &ToolError{
			Code:      "not_implemented",
			Message:   notImplementedMessage,
			Retryable: false,
		},
	}, nil
}

func await(_ context.Context, _ *mcp.CallToolRequest, _ AwaitInput) (*mcp.CallToolResult, AwaitOutput, error) {
	return &mcp.CallToolResult{IsError: true}, AwaitOutput{
		Status:   "not_implemented",
		Terminal: true,
		Error: &ToolError{
			Code:      "not_implemented",
			Message:   notImplementedMessage,
			Retryable: false,
		},
	}, nil
}
