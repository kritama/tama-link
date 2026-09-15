package upstream

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// metaTriple is the SEP-2575 per-request _meta carried on every outgoing
// request. Values are fixed for the client's lifetime; per-request
// declaration means every request carries them, never only the first.
type metaTriple struct {
	ProtocolVersion    string             `json:"io.modelcontextprotocol/protocolVersion"`
	ClientInfo         mcp.Implementation `json:"io.modelcontextprotocol/clientInfo"`
	ClientCapabilities json.RawMessage    `json:"io.modelcontextprotocol/clientCapabilities"`
}

// buildMeta encodes the client's _meta triple. Capabilities must be a JSON
// object; it is encoded verbatim so declared extension objects survive
// unchanged.
func buildMeta(clientInfo mcp.Implementation, capabilities json.RawMessage) (json.RawMessage, error) {
	if !isJSONObject(capabilities) {
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

// isJSONObject reports whether raw is a valid JSON object document.
func isJSONObject(raw json.RawMessage) bool {
	var v map[string]json.RawMessage
	return json.Unmarshal(raw, &v) == nil
}
