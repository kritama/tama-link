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

func submitHandler(allowed []string, operation submitOperation) mcp.ToolHandler {
	allowedTools := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		allowedTools[name] = true
	}

	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		input, err := contract.DecodeSubmitInput(request.Params.Arguments)
		if err != nil {
			return contractErrorResult(contract.CodeInvalidRequest, err.Error())
		}
		if !allowedTools[input.Tool] {
			return contractErrorResult(
				contract.CodeOperationNotAllowed,
				fmt.Sprintf("operation %q is not allowed by the selected profile", input.Tool),
			)
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
