package tama2026

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/upstream"
)

// tasksCapabilities declares the Tasks extension for one tools/call.
var tasksCapabilities = json.RawMessage(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`)

// emptyCapabilities declares no extensions.
var emptyCapabilities = json.RawMessage(`{}`)

// Execute issues one tools/call for a pinned upstream_task operation. The
// Tasks capability is declared per request from the pinned task support,
// and the expected result shape is enforced on the response. Local
// strategies are executed by the leased worker, never here.
func (cn *Connection) Execute(ctx context.Context, name string, args json.RawMessage) (*upstream.CallToolResponse, error) {
	d, ok := cn.catalog.Find(name)
	if !ok {
		return nil, fmt.Errorf("%w: tool %q is not in the effective catalog", ErrOperationNotAllowed, name)
	}
	if d.Strategy != catalog.StrategyUpstreamTask {
		return nil, fmt.Errorf("%w: tool %q uses strategy %s", ErrOperationNotAllowed, name, d.Strategy)
	}
	resp, err := cn.adapter.upstream.CallTool(ctx, &upstream.CallToolParams{
		Name:         name,
		Arguments:    args,
		Capabilities: capabilitiesFor(d.TaskSupport),
	})
	if err != nil {
		return nil, classify(err)
	}
	if err := checkResultShape(d, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// capabilitiesFor renders the per-request client capabilities for one
// pinned task support.
func capabilitiesFor(s catalog.TaskSupport) json.RawMessage {
	if s == catalog.TaskSupportForbidden {
		return emptyCapabilities
	}
	return tasksCapabilities
}

// checkResultShape enforces the resultType the pinned contract expects:
// required tasks must come back as tasks, forbidden tools must never do,
// and optional tools may use either shape.
func checkResultShape(d catalog.Descriptor, resp *upstream.CallToolResponse) error {
	switch d.TaskSupport {
	case catalog.TaskSupportRequired:
		if !resp.IsTask() {
			return fmt.Errorf("%w: tool %q requires a task result", ErrProtocolMismatch, d.Name)
		}
	case catalog.TaskSupportForbidden:
		if resp.IsTask() {
			return fmt.Errorf("%w: tool %q returned a task result", ErrUnexpectedTaskResult, d.Name)
		}
	}
	return nil
}
