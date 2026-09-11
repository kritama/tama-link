package catalog

import (
	"bytes"
	"encoding/json"
)

// canonical returns the deterministic encoding of a raw JSON value: compact
// form with sorted object keys. An empty value encodes as null.
func canonical(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// LiveTool is the security-relevant projection of one live upstream tool,
// normalized by the upstream adapter.
type LiveTool struct {
	Name        string
	InputSchema json.RawMessage
	Annotations map[string]any
	TaskSupport bool
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
