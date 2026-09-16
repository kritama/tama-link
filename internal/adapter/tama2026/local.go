package tama2026

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/upstream"
)

// ExecuteLocal executes one pinned local_replayable operation as an ordinary
// synchronous tools/call under the leased local worker. No Tasks capability
// is declared and only a complete CallToolResult is accepted: a task-shaped
// result for a synchronous profile operation is a pinned-contract mismatch,
// never a polled task.
func (cn *Connection) ExecuteLocal(ctx context.Context, name string, args json.RawMessage) (*contract.Result, error) {
	d, ok := cn.catalog.Find(name)
	if !ok {
		return nil, fmt.Errorf("%w: tool %q is not in the effective catalog", ErrOperationNotAllowed, name)
	}
	if d.Strategy != catalog.StrategyLocalReplayable {
		return nil, fmt.Errorf("%w: tool %q uses strategy %s", ErrOperationNotAllowed, name, d.Strategy)
	}
	resp, err := cn.adapter.upstream.CallTool(ctx, &upstream.CallToolParams{
		Name:         name,
		Arguments:    args,
		Capabilities: emptyCapabilities,
	})
	if err != nil {
		return nil, classify(err)
	}
	if resp.IsTask() {
		return nil, fmt.Errorf("%w: synchronous tool %q returned a task result", ErrCatalogMismatch, d.Name)
	}
	result, err := NormalizeCompleteResult(resp.Raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProtocolMismatch, err)
	}
	return &result, nil
}

// NormalizeCompleteResult validates one complete CallToolResult document and
// projects it to the lossless normalized result. Content blocks, structured
// content, and _meta stay raw JSON; the store enforces the size bound.
func NormalizeCompleteResult(raw json.RawMessage) (contract.Result, error) {
	var view struct {
		ResultType        string            `json:"resultType"`
		IsError           bool              `json:"isError"`
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
		Meta              json.RawMessage   `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return contract.Result{}, fmt.Errorf("complete result is not a JSON object")
	}
	if view.ResultType != "complete" {
		return contract.Result{}, fmt.Errorf("complete result has resultType %q", view.ResultType)
	}
	result := contract.Result{
		IsError:           view.IsError,
		Content:           view.Content,
		StructuredContent: view.StructuredContent,
		Meta:              view.Meta,
	}
	if err := result.Validate(); err != nil {
		return contract.Result{}, err
	}
	return result, nil
}
