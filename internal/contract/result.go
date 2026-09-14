package contract

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/kritama/tama-link/internal/jsonvalue"
)

// Validate checks the stable structural invariants while retaining every raw
// MCP content field. Endpoint adapters remain responsible for protocol-version
// validation before constructing the normalized result.
func (r Result) Validate() error {
	if r.Content == nil {
		return fmt.Errorf("content is required")
	}
	for index, block := range r.Content {
		canonical, err := jsonvalue.Canonical(block)
		if err != nil || len(bytes.TrimSpace(canonical)) == 0 || bytes.TrimSpace(canonical)[0] != '{' {
			return fmt.Errorf("content block %d is not a JSON object", index)
		}
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(canonical, &header); err != nil || header.Type == "" {
			return fmt.Errorf("content block %d has no type", index)
		}
	}
	if len(r.StructuredContent) > 0 {
		if _, err := jsonvalue.Canonical(r.StructuredContent); err != nil {
			return fmt.Errorf("structured_content is not valid unambiguous JSON: %w", err)
		}
	}
	if len(r.Meta) > 0 {
		canonical, err := jsonvalue.Canonical(r.Meta)
		trimmed := bytes.TrimSpace(canonical)
		if err != nil || len(trimmed) == 0 || trimmed[0] != '{' {
			return fmt.Errorf("result _meta is not a JSON object")
		}
	}
	return nil
}
