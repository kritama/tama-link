package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// rewriteID replaces exact JSON string values equal to from with to. It does
// not re-encode the document, so number literals survive.
func rewriteID(raw json.RawMessage, from, to string) []byte {
	if from == "" || from == to {
		return append([]byte(nil), raw...)
	}
	quotedFrom, err := json.Marshal(from)
	if err != nil {
		return append([]byte(nil), raw...)
	}
	quotedTo, err := json.Marshal(to)
	if err != nil {
		return append([]byte(nil), raw...)
	}
	return bytes.ReplaceAll(raw, quotedFrom, quotedTo)
}

// sseData frames one JSON payload as a single SSE event. The payload is
// compacted first: a pretty-printed fixture document contains newlines,
// which the event-stream grammar would treat as extra fields.
func sseData(payload []byte) []byte {
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		compact.Write(payload)
	}
	var out bytes.Buffer
	out.WriteString("data: ")
	out.Write(compact.Bytes())
	out.WriteString("\n\n")
	return out.Bytes()
}

// canonical strips _meta and renders stable JSON for result comparison.
func canonical(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("decode result: %w", err)
	}
	stripMeta(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}
	return string(encoded), nil
}

func stripMeta(value any) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	delete(object, "_meta")
}

func sameResult(got, want json.RawMessage) error {
	left, err := canonical(got)
	if err != nil {
		return err
	}
	right, err := canonical(want)
	if err != nil {
		return err
	}
	if left != right {
		return fmt.Errorf("result %s, fixture %s", left, right)
	}
	return nil
}
