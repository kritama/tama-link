// Package server defines Tama Link's client-facing MCP server.
package server

import (
	"context"
	"errors"

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
)

// App is the application service behind the two downstream tools. Handlers
// only validate and translate: persistence, upstream transport, OAuth, and
// scheduling live in the service and its dependencies.
type App interface {
	Submit(ctx context.Context, in contract.SubmitInput) (contract.SubmitOutput, *contract.Error)
	Await(ctx context.Context, in contract.AwaitInput) (contract.AwaitOutput, *contract.Error)
}

// New creates a client-facing MCP server for one validated profile with
// exactly the submit and await tools. The application is required: a server
// cannot advertise those tools without a service that implements them.
func New(p *profile.Profile, buildVersion string, app App) (*mcp.Server, error) {
	if p == nil {
		return nil, errors.New("profile is required")
	}
	if app == nil {
		return nil, errors.New("application is required")
	}
	ops := p.Catalog().Callable()

	instance := mcp.NewServer(
		&mcp.Implementation{Name: serverName, Version: buildVersion},
		&mcp.ServerOptions{Instructions: composeInstructions(p.Instructions)},
	)
	instance.AddTool(&mcp.Tool{
		Name:        contract.ToolSubmit,
		Description: composeSubmitDescription(ops),
		InputSchema: submitInputSchema(),
	}, submitHandler(appSubmitOperation(app)))
	instance.AddTool(&mcp.Tool{
		Name:        contract.ToolAwait,
		Description: awaitDescription,
		InputSchema: awaitInputSchema(),
	}, awaitHandler(appAwaitOperation(app)))
	return instance, nil
}

// awaitInputSchema describes the stable await fields. input_responses is a
// free-form object keyed by outstanding input-request identifiers; its values
// are validated against the request schema at the application boundary.
func awaitInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"submission_id": map[string]any{
				"type":        "string",
				"description": "identifier returned by submit",
			},
			"timeout_ms": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": "maximum wait for this call; zero uses the profile default",
			},
			"cursor": map[string]any{
				"type":        "string",
				"description": "opaque progress cursor from a previous await; replay from this cursor",
			},
			"input_responses": map[string]any{
				"type":        "object",
				"description": "responses to outstanding input_required requests, keyed by input request identifier",
			},
		},
		"required": []string{"submission_id"},
	}
}

func appSubmitOperation(app App) submitOperation {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in contract.SubmitInput) (*mcp.CallToolResult, any, error) {
		out, appErr := app.Submit(ctx, in)
		if appErr != nil {
			return &mcp.CallToolResult{IsError: true}, contract.ErrorOutput{Error: appErr}, nil
		}
		return nil, out, nil
	}
}

func appAwaitOperation(app App) awaitOperation {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in contract.AwaitInput) (*mcp.CallToolResult, any, error) {
		out, appErr := app.Await(ctx, in)
		if appErr != nil {
			return &mcp.CallToolResult{IsError: true}, contract.ErrorOutput{Error: appErr}, nil
		}
		return nil, out, nil
	}
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

// submitInputSchema describes the stable submit fields. The tool name is
// deliberately not constrained to the current catalog: an exact retry of
// an accepted request must reach the application, where idempotency
// reconciliation runs before the catalog check, and the catalog check
// enforces the profile only for genuinely new work. The approved
// operations stay advertised in the tool description.
func submitInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tool": map[string]any{
				"type":        "string",
				"description": "upstream Tama tool name submitted against the selected profile",
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
