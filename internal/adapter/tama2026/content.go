package tama2026

import (
	"encoding/json"
	"fmt"
)

// validateContentBlocks enforces the required fields of the pinned
// protocol version's known content block types before a complete result
// can be persisted and exposed downstream: the contract assigns
// protocol-version wire validation to the endpoint adapter, and the
// normalized result's own validation only checks that every block is an
// object with a nonempty type. Unknown types are preserved as extension
// blocks and pass — forward compatibility is the upstream's to declare.
func validateContentBlocks(blocks []json.RawMessage) error {
	for index, block := range blocks {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(block, &fields); err != nil {
			return fmt.Errorf("content block %d is not a JSON object", index)
		}
		var header struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(block, &header)
		var err error
		switch header.Type {
		case "text":
			err = requireStrings(fields, "text")
		case "image", "audio":
			err = requireStrings(fields, "data", "mimeType")
		case "resource_link":
			err = requireStrings(fields, "uri", "name")
		case "resource":
			err = validateEmbeddedResource(fields)
		}
		if err != nil {
			return fmt.Errorf("content block %d (%s): %w", index, header.Type, err)
		}
	}
	return nil
}

// requireStrings reports whether every named field is present and a JSON
// string.
func requireStrings(fields map[string]json.RawMessage, names ...string) error {
	for _, name := range names {
		raw, ok := fields[name]
		if !ok {
			return fmt.Errorf("missing required field %q", name)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("field %q is not a string", name)
		}
	}
	return nil
}

// validateEmbeddedResource enforces the embedded resource block: a
// resource object with a uri and a text or blob payload.
func validateEmbeddedResource(fields map[string]json.RawMessage) error {
	raw, ok := fields["resource"]
	if !ok {
		return fmt.Errorf("missing required field \"resource\"")
	}
	var resource map[string]json.RawMessage
	if err := json.Unmarshal(raw, &resource); err != nil {
		return fmt.Errorf("field \"resource\" is not an object")
	}
	if err := requireStrings(resource, "uri"); err != nil {
		return fmt.Errorf("embedded resource: %w", err)
	}
	if _, hasText := resource["text"]; !hasText {
		if _, hasBlob := resource["blob"]; !hasBlob {
			return fmt.Errorf("embedded resource carries neither text nor blob")
		}
	}
	return nil
}
