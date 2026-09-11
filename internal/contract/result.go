package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Validate checks the stable structural invariants while retaining every raw
// MCP content field. Endpoint adapters remain responsible for protocol-version
// validation before constructing the normalized result.
func (r Result) Validate() error {
	for index, block := range r.Content {
		if !json.Valid(block) || len(bytes.TrimSpace(block)) == 0 || bytes.TrimSpace(block)[0] != '{' {
			return fmt.Errorf("content block %d is not a JSON object", index)
		}
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(block, &header); err != nil || header.Type == "" {
			return fmt.Errorf("content block %d has no type", index)
		}
	}
	if len(r.StructuredContent) > 0 && !json.Valid(r.StructuredContent) {
		return fmt.Errorf("structured_content is not valid JSON")
	}
	if len(r.Meta) > 0 {
		trimmed := bytes.TrimSpace(r.Meta)
		if !json.Valid(trimmed) || len(trimmed) == 0 || trimmed[0] != '{' {
			return fmt.Errorf("result _meta is not a JSON object")
		}
	}
	return nil
}
