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
// never a polled task. maxResponseBytes, when positive, bounds this one
// response instead of the upstream client's configured bound; callers pass
// the submission's accepted lifecycle bound so a recovered execution runs
// under the policy it was accepted with.
func (cn *Connection) ExecuteLocal(ctx context.Context, name string, args json.RawMessage, maxResponseBytes int64) (*contract.Result, error) {
	d, ok := cn.catalog.Find(name)
	if !ok {
		return nil, fmt.Errorf("%w: tool %q is not in the effective catalog", ErrOperationNotAllowed, name)
	}
	if d.Strategy != catalog.StrategyLocalReplayable {
		return nil, fmt.Errorf("%w: tool %q uses strategy %s", ErrOperationNotAllowed, name, d.Strategy)
	}
	resp, err := cn.adapter.upstream.CallTool(ctx, &upstream.CallToolParams{
		Name:             name,
		Arguments:        args,
		Capabilities:     emptyCapabilities,
		MaxResponseBytes: maxResponseBytes,
	})
	if err != nil {
		return nil, classify(err)
	}
	if resp.IsTask() {
		return nil, fmt.Errorf("%w: synchronous tool %q returned a task result", ErrUnexpectedTaskResult, d.Name)
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
		IsError           *bool             `json:"isError"`
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
	// isError is a required CallToolResult field: a missing or null value
	// must be a protocol failure, never silently read as a success.
	if view.IsError == nil {
		return contract.Result{}, fmt.Errorf("complete result omits the required isError field")
	}
	result := contract.Result{
		IsError:           *view.IsError,
		Content:           view.Content,
		StructuredContent: view.StructuredContent,
		Meta:              view.Meta,
	}
	if err := result.Validate(); err != nil {
		return contract.Result{}, err
	}
	return result, nil
}
