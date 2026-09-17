package upstream

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP methods and notifications on the TamaMCP 2026-07-28 wire contract.
const (
	MethodDiscover  = "server/discover"
	MethodListTools = "tools/list"
	MethodCallTool  = "tools/call"
)

// DiscoverResult is the server/discover reply.
type DiscoverResult struct {
	// Raw is the complete result value, lossless.
	Raw json.RawMessage
	// SupportedVersions lists the protocol versions the server accepts.
	SupportedVersions []string
	// Capabilities is the raw capabilities object; extension declarations
	// survive verbatim.
	Capabilities json.RawMessage
	// Instructions is the server instruction text.
	Instructions string
	// ServerInfo is the server identity from result _meta, when present.
	ServerInfo *mcp.Implementation
}

// HasTaskExtension reports whether the server declared the Tasks extension
// with the required JSON object value. A missing or malformed declaration
// (null, scalar, array) is not a declaration.
func (r *DiscoverResult) HasTaskExtension() bool {
	if !IsJSONObject(r.Capabilities) {
		return false
	}
	var caps struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if json.Unmarshal(r.Capabilities, &caps) != nil {
		return false
	}
	ext, ok := caps.Extensions["io.modelcontextprotocol/tasks"]
	return ok && IsJSONObject(ext)
}

// Discover performs server/discover. The client never sends initialize; this
// is the only handshake, and any failure is terminal for the connection
// attempt. maxResponseBytes bounds the response; bootstrap callers pass the
// implementation hard ceiling so a later, lower profile bound can never
// constrain connection establishment.
func (c *Client) Discover(ctx context.Context, maxResponseBytes int64) (*DiscoverResult, error) {
	raw, err := c.call(ctx, MethodDiscover, "", json.RawMessage("{}"), maxResponseBytes)
	if err != nil {
		return nil, err
	}
	return decodeDiscover(raw)
}

// decodeDiscover projects the discover result, keeping capabilities raw.
func decodeDiscover(raw json.RawMessage) (*DiscoverResult, error) {
	var view struct {
		SupportedVersions []string                   `json:"supportedVersions"`
		Capabilities      json.RawMessage            `json:"capabilities"`
		Instructions      string                     `json:"instructions"`
		Meta              map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("decode discover result: %w", err)
	}
	if len(view.SupportedVersions) == 0 {
		return nil, newError(KindProtocol, 0, fmt.Errorf("discover result has no supported versions"))
	}
	result := &DiscoverResult{
		Raw:               raw,
		SupportedVersions: view.SupportedVersions,
		Capabilities:      view.Capabilities,
		Instructions:      view.Instructions,
	}
	if serverInfo, ok := view.Meta[mcp.MetaKeyServerInfo]; ok {
		var info mcp.Implementation
		if err := json.Unmarshal(serverInfo, &info); err != nil {
			return nil, newError(KindProtocol, 0, fmt.Errorf("decode discover server info"))
		}
		result.ServerInfo = &info
	}
	return result, nil
}

// LiveTool is one entry of a live tools/list page. Schemas remain raw JSON
// so number literals survive canonical comparison.
type LiveTool struct {
	// Raw is the complete tool object, lossless.
	Raw json.RawMessage
	// Name identifies the tool.
	Name string
	// Title is the optional display name.
	Title string
	// Description is the tool description.
	Description string
	// Annotations is the raw annotations object, when present.
	Annotations json.RawMessage
	// InputSchema is the raw input schema.
	InputSchema json.RawMessage
	// OutputSchema is the raw output schema, when present.
	OutputSchema json.RawMessage
}

// ListToolsResult is one tools/list page.
type ListToolsResult struct {
	// Raw is the complete result value, lossless.
	Raw json.RawMessage
	// Tools holds the page tools in server order.
	Tools []*LiveTool
	// NextCursor continues the listing when non-empty.
	NextCursor string
}

// ListTools reads one tools/list page.
func (c *Client) ListTools(ctx context.Context, cursor string, maxResponseBytes int64) (*ListToolsResult, error) {
	wire, err := json.Marshal(wireListTools{Cursor: cursor})
	if err != nil {
		return nil, fmt.Errorf("encode tools/list params: %w", err)
	}
	raw, err := c.call(ctx, MethodListTools, "", wire, maxResponseBytes)
	if err != nil {
		return nil, err
	}
	return decodeListTools(raw)
}

// ListAllTools follows NextCursor until the listing is exhausted, bounded by
// a hard page ceiling so a hostile cursor loop cannot run forever.
func (c *Client) ListAllTools(ctx context.Context, maxResponseBytes int64) ([]*LiveTool, error) {
	const maxPages = 32
	bound := c.resolveBound(maxResponseBytes)
	var all []*LiveTool
	var catalogBytes int64
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, newError(KindProtocol, 0, fmt.Errorf("tools/list exceeds %d pages", maxPages))
		}
		result, err := c.ListTools(ctx, cursor, maxResponseBytes)
		if err != nil {
			return nil, err
		}
		// The cumulative catalog is bounded, not just each page: a hostile
		// endpoint serving many just-under-bound pages cannot exhaust
		// process memory through pagination.
		for _, tool := range result.Tools {
			catalogBytes += int64(len(tool.Raw))
		}
		if catalogBytes > bound {
			return nil, newError(KindTooLarge, 0, fmt.Errorf("tools/list exceeds the cumulative %d byte catalog bound", bound))
		}
		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			return all, nil
		}
		cursor = result.NextCursor
	}
}

// wireListTools is the tools/list params object. The _meta triple is merged
// in by the client, so number literals in other fields survive untouched.
type wireListTools struct {
	Cursor string `json:"cursor,omitempty"`
}

// decodeListTools projects one tools/list page, keeping tool documents raw.
func decodeListTools(raw json.RawMessage) (*ListToolsResult, error) {
	var view struct {
		Tools      []json.RawMessage `json:"tools"`
		NextCursor string            `json:"nextCursor"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("decode tools/list result: %w", err)
	}
	tools := make([]*LiveTool, 0, len(view.Tools))
	for _, toolRaw := range view.Tools {
		tool, err := decodeLiveTool(toolRaw)
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return &ListToolsResult{Raw: raw, Tools: tools, NextCursor: view.NextCursor}, nil
}

// decodeLiveTool projects one tool document.
func decodeLiveTool(raw json.RawMessage) (*LiveTool, error) {
	var view struct {
		Name         string          `json:"name"`
		Title        string          `json:"title"`
		Description  string          `json:"description"`
		Annotations  json.RawMessage `json:"annotations"`
		InputSchema  json.RawMessage `json:"inputSchema"`
		OutputSchema json.RawMessage `json:"outputSchema"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("decode tool document: %w", err)
	}
	if view.Name == "" {
		return nil, newError(KindProtocol, 0, fmt.Errorf("tool document has no name"))
	}
	return &LiveTool{
		Raw:          raw,
		Name:         view.Name,
		Title:        view.Title,
		Description:  view.Description,
		Annotations:  view.Annotations,
		InputSchema:  view.InputSchema,
		OutputSchema: view.OutputSchema,
	}, nil
}
