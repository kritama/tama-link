package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// canonical returns the deterministic encoding of exactly one complete JSON
// value: compact form, sorted object keys, and numeric literals preserved
// without float conversion. Trailing data and duplicate object keys are
// rejected. Distinct numeric literals are never collapsed, so comparisons
// fail closed rather than miss a change.
func canonical(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	value, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// decodeValue reads one complete JSON value, preserving numeric literals and
// rejecting duplicate object keys.
func decodeValue(dec *json.Decoder) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case nil:
		return nil, nil
	case bool:
		return value, nil
	case string:
		return value, nil
	case json.Number:
		return value, nil
	case json.Delim:
		return decodeDelimited(dec, value)
	default:
		return nil, fmt.Errorf("unsupported JSON token %T", token)
	}
}

func decodeDelimited(dec *json.Decoder, open json.Delim) (any, error) {
	switch open {
	case '[':
		var slice []any
		for dec.More() {
			item, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			slice = append(slice, item)
		}
		return slice, consumeClosing(dec, ']')
	case '{':
		object := map[string]any{}
		seen := make(map[string]bool)
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key is not a string")
			}
			if seen[key] {
				return nil, fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = true
			item, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			object[key] = item
		}
		return object, consumeClosing(dec, '}')
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", open)
	}
}

func consumeClosing(dec *json.Decoder, want json.Delim) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != want {
		return fmt.Errorf("unexpected token after JSON value")
	}
	return nil
}

// LiveTool is the security-relevant projection of one live upstream tool,
// normalized by the upstream adapter. An absent live execution declaration
// must be normalized to TaskSupportForbidden before comparison.
type LiveTool struct {
	Name         string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Annotations  map[string]any
	TaskSupport  TaskSupport
}

// Drift reports the security-relevant fields in which the live tool differs
// from the pinned descriptor. Title and description are display fields and
// are not part of the security-relevant comparison. A nil result means the
// live tool matches its pinned contract.
func (d Descriptor) Drift(live LiveTool) []string {
	var drifted []string
	if d.Name != live.Name {
		drifted = append(drifted, "name")
	}
	if !canonicalEqual(d.InputSchema, live.InputSchema) {
		drifted = append(drifted, "input_schema")
	}
	if !canonicalEqual(d.OutputSchema, live.OutputSchema) {
		drifted = append(drifted, "output_schema")
	}
	if !sameValue(normalizeMap(d.Annotations), normalizeMap(live.Annotations)) {
		drifted = append(drifted, "annotations")
	}
	if d.TaskSupport != live.TaskSupport {
		drifted = append(drifted, "task_support")
	}
	return drifted
}

// canonicalEqual reports whether two raw JSON values are semantically equal
// after canonicalization.
func canonicalEqual(a, b json.RawMessage) bool {
	ea, err := canonical(a)
	if err != nil {
		return false
	}
	eb, err := canonical(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ea, eb)
}

// normalizeMap treats a nil or empty map as absent so that both encode as
// null.
func normalizeMap(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func sameValue(a, b any) bool {
	ea, err := json.Marshal(a)
	if err != nil {
		return false
	}
	eb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ea, eb)
}
