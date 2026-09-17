package server

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/contract"
)

type awaitOperation func(
	context.Context,
	*mcp.CallToolRequest,
	contract.AwaitInput,
) (*mcp.CallToolResult, any, error)

func awaitHandler(operation awaitOperation) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Decode the raw arguments: input_responses values must keep their
		// exact JSON bytes, so the SDK's map[string]any normalization is not
		// allowed on this path.
		input, err := contract.DecodeAwaitInput(request.Params.Arguments)
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
