package upstream

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// metaTriple is the SEP-2575 per-request _meta carried on every outgoing
// request. The triple is declared on each request, never only the first.
// Task methods additionally ensure the Tasks extension is present, because
// that declaration is per applicable request and is not inferred from
// discovery or from the client's default capability set.
type metaTriple struct {
	ProtocolVersion    string             `json:"io.modelcontextprotocol/protocolVersion"`
	ClientInfo         mcp.Implementation `json:"io.modelcontextprotocol/clientInfo"`
	ClientCapabilities json.RawMessage    `json:"io.modelcontextprotocol/clientCapabilities"`
}

// buildMeta encodes the client's _meta triple.
func buildMeta(clientInfo mcp.Implementation, capabilities json.RawMessage) (json.RawMessage, error) {
	return buildMetaWith(clientInfo, capabilities)
}

// buildMetaWith encodes the _meta triple with an explicit capabilities
// object. Capabilities must be a JSON object; it is encoded verbatim so
// declared extension objects survive unchanged.
func buildMetaWith(clientInfo mcp.Implementation, capabilities json.RawMessage) (json.RawMessage, error) {
	if !IsJSONObject(capabilities) {
		return nil, fmt.Errorf("client capabilities must be a JSON object")
	}
	meta, err := json.Marshal(metaTriple{
		ProtocolVersion:    protocolVersion,
		ClientInfo:         clientInfo,
		ClientCapabilities: capabilities,
	})
	if err != nil {
		return nil, fmt.Errorf("encode client meta: %w", err)
	}
	return meta, nil
}

// tasksExtension is the Tasks capability key declared on applicable requests.
const tasksExtension = "io.modelcontextprotocol/tasks"

// tasksMeta returns the _meta triple for one task method or subscription.
// It preserves a client that already declares the Tasks extension and
// otherwise adds that declaration without dropping other capabilities.
func (c *Client) tasksMeta() (json.RawMessage, error) {
	if declaresTasks(c.meta) {
		return c.meta, nil
	}
	caps, err := capabilitiesFromMeta(c.meta)
	if err != nil {
		return nil, err
	}
	merged, err := withTasksCapability(caps)
	if err != nil {
		return nil, err
	}
	return buildMetaWith(c.info, merged)
}

// declaresTasks reports whether meta already carries a Tasks extension object.
func declaresTasks(meta json.RawMessage) bool {
	caps, err := capabilitiesFromMeta(meta)
	if err != nil {
		return false
	}
	var view struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if json.Unmarshal(caps, &view) != nil {
		return false
	}
	ext, ok := view.Extensions[tasksExtension]
	return ok && IsJSONObject(ext)
}

// capabilitiesFromMeta extracts the client capabilities object.
func capabilitiesFromMeta(meta json.RawMessage) (json.RawMessage, error) {
	var view struct {
		Caps json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
	}
	if err := json.Unmarshal(meta, &view); err != nil || !IsJSONObject(view.Caps) {
		return nil, fmt.Errorf("client meta is missing capabilities")
	}
	return view.Caps, nil
}

// withTasksCapability returns capabilities that declare the Tasks extension.
func withTasksCapability(caps json.RawMessage) (json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(caps, &doc); err != nil {
		return nil, fmt.Errorf("decode client capabilities: %w", err)
	}
	extensions := map[string]json.RawMessage{}
	if raw, ok := doc["extensions"]; ok && string(raw) != "null" {
		if !IsJSONObject(raw) {
			return nil, fmt.Errorf("client capabilities extensions are not an object")
		}
		if err := json.Unmarshal(raw, &extensions); err != nil {
			return nil, fmt.Errorf("decode client capability extensions: %w", err)
		}
	}
	if ext, ok := extensions[tasksExtension]; !ok || !IsJSONObject(ext) {
		extensions[tasksExtension] = json.RawMessage(`{}`)
	}
	encoded, err := json.Marshal(extensions)
	if err != nil {
		return nil, fmt.Errorf("encode client capability extensions: %w", err)
	}
	doc["extensions"] = encoded
	merged, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode client capabilities: %w", err)
	}
	return merged, nil
}

// IsJSONObject reports whether raw is a valid JSON object document. JSON
// null and scalar documents are not objects.
func IsJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return false
	}
	var v map[string]json.RawMessage
	return json.Unmarshal(raw, &v) == nil
}
