package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/contract"
)

type submitOperation func(
	context.Context,
	*mcp.CallToolRequest,
	contract.SubmitInput,
) (*mcp.CallToolResult, any, error)

// submitHandler decodes and translates the request and routes it to the
// application. The handler deliberately does not enforce the profile
// catalog: an exact retry of an accepted request must reach the
// application, where idempotency reconciliation runs before the catalog
// check, and the application enforces the catalog for genuinely new work.
func submitHandler(operation submitOperation) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		input, err := contract.DecodeSubmitInput(request.Params.Arguments)
		if err != nil {
			return contractErrorResult(contract.CodeInvalidRequest, err.Error())
		}

		result, output, err := operation(ctx, request, input)
		if err != nil {
			return nil, err
		}
		return attachStructuredOutput(result, output)
	}
}

func attachStructuredOutput(result *mcp.CallToolResult, output any) (*mcp.CallToolResult, error) {
	if result == nil {
		result = &mcp.CallToolResult{}
	}
	if output == nil {
		return result, nil
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("encode submit output: %w", err)
	}
	raw := json.RawMessage(encoded)
	result.StructuredContent = raw
	if result.Content == nil {
		result.Content = []mcp.Content{&mcp.TextContent{Text: string(raw)}}
	}
	return result, nil
}

func contractErrorResult(code contract.Code, message string) (*mcp.CallToolResult, error) {
	contractErr := contract.NewError(code, message)
	return attachStructuredOutput(
		&mcp.CallToolResult{IsError: true},
		contract.ErrorOutput{Error: &contractErr},
	)
}
