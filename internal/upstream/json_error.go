package upstream

import (
	"bytes"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// jsonRPCErrorCode extracts a JSON-RPC error code from a non-success body.
// A missing response id still carries the code: some pinned error documents
// omit it, and the status alone would hide the protocol failure.
func jsonRPCErrorCode(data []byte) (int, bool) {
	if len(bytes.TrimSpace(data)) == 0 {
		return 0, false
	}
	if msg, err := jsonrpc.DecodeMessage(data); err == nil {
		if werr, ok := asWireError(msg); ok {
			return int(werr.Code), true
		}
	}
	var body struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) != nil || body.Error == nil {
		return 0, false
	}
	return body.Error.Code, true
}
